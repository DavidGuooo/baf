package mailbox

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/DavidGuooo/baf/internal/envelope"
)

// Files (ADR-0007). The bytes of every file sent or received are kept under
// the mailbox's FilesDir. A message carries only each file's name, hash and
// size; the bytes are handed over ahead of it.

// FileInfo is one of a stored message's files and where it is on this machine.
type FileInfo struct {
	envelope.File
	Path string `json:"path"`
}

// AddFile keeps the bytes of a file one of this node's agents is about to
// send, and returns what the message should say about it. The command
// streams the bytes, so the mailbox never opens a path an agent chose.
func (m *Mailbox) AddFile(name string, r io.Reader) (envelope.File, error) {
	if err := envelope.CheckFileName(name); err != nil {
		return envelope.File{}, &Rejection{Reason: err.Error()}
	}
	return m.writeFile(name, nil, r)
}

// StoreFile keeps bytes another node handed over ahead of a message. It
// checks them against f and returns a *Rejection when they don't match or
// break a limit.
func (m *Mailbox) StoreFile(f envelope.File, r io.Reader) error {
	if err := envelope.CheckFileName(f.Name); err != nil {
		return &Rejection{Reason: err.Error()}
	}
	if f.Size < 0 || f.Size > envelope.MaxFileSize {
		return reject("file %q: size %d is outside the limit of %d bytes", f.Name, f.Size, envelope.MaxFileSize)
	}
	_, err := m.writeFile(f.Name, &f, r)
	return err
}

// openFile opens the bytes of a file this mailbox holds, for the outbox to
// hand over.
func (m *Mailbox) openFile(sha256 string) (*os.File, error) {
	return os.Open(filepath.Join(m.filesDir, "blobs", sha256))
}

// writeFile streams r into blobs/, hashing as it goes, and checks the result
// against want when there is one. A blob already held is kept as it is.
func (m *Mailbox) writeFile(name string, want *envelope.File, r io.Reader) (envelope.File, error) {
	dir := filepath.Join(m.filesDir, "blobs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return envelope.File{}, err
	}
	tmp, err := os.CreateTemp(dir, ".upload-")
	if err != nil {
		return envelope.File{}, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(r, envelope.MaxFileSize+1))
	if err != nil {
		return envelope.File{}, err
	}
	if n > envelope.MaxFileSize {
		return envelope.File{}, reject("file %q exceeds the limit of %d bytes", name, envelope.MaxFileSize)
	}
	f := envelope.File{Name: name, SHA256: fmt.Sprintf("%x", hash.Sum(nil)), Size: n}
	if want != nil {
		if n != want.Size {
			return envelope.File{}, reject("file %q: size mismatch: got %d bytes, expected %d", name, n, want.Size)
		}
		if f.SHA256 != want.SHA256 {
			return envelope.File{}, reject("file %q: sha256 mismatch: got %s, expected %s", name, f.SHA256, want.SHA256)
		}
	}
	if err := tmp.Sync(); err != nil {
		return envelope.File{}, err
	}
	if err := tmp.Close(); err != nil {
		return envelope.File{}, err
	}
	m.filesMu.Lock()
	defer m.filesMu.Unlock()
	path := filepath.Join(dir, f.SHA256)
	if _, err := os.Stat(path); err == nil {
		// A new send may reuse old bytes. Give it a day to queue the message.
		now := m.now()
		if err := os.Chtimes(path, now, now); err != nil {
			return envelope.File{}, err
		}
		return f, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return envelope.File{}, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return envelope.File{}, err
	}
	for _, p := range []string{dir, m.filesDir, filepath.Dir(m.filesDir)} {
		if err := syncDir(p); err != nil {
			return envelope.File{}, err
		}
	}
	return f, nil
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// filePath is where an agent finds a file of message id from node.
func (m *Mailbox) filePath(node, id, name string) string {
	return filepath.Join(m.filesDir, "in", node, id, name)
}

// checkBlobs refuses a message naming a file whose bytes this mailbox doesn't
// hold. local says the sender is one of this node's own agents.
func (m *Mailbox) checkBlobs(files []envelope.File, local bool) error {
	for _, f := range files {
		info, err := os.Stat(filepath.Join(m.filesDir, "blobs", f.SHA256))
		if errors.Is(err, os.ErrNotExist) {
			if local {
				return reject("file %q wasn't added to this mailbox; send it with --file", f.Name)
			}
			return reject("file %q wasn't handed over before the message", f.Name)
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() != f.Size {
			return reject("file %q: stored blob size does not match %d bytes", f.Name, f.Size)
		}
	}
	return nil
}

// placeFiles puts each of a message's files at its filePath, linked from its
// blob, before the message is stored. On a retry, files already there stay.
func (m *Mailbox) placeFiles(node, id string, files []envelope.File) error {
	if len(files) == 0 {
		return nil
	}
	if _, err := envelope.ParseAddress(node + "/x"); err != nil {
		return reject("bad sender node for files: %s", err)
	}
	if err := envelope.CheckFileName(id); err != nil {
		return reject("bad message id for files: %s", err)
	}
	if err := m.checkBlobs(files, false); err != nil {
		return err
	}
	dir := filepath.Dir(m.filePath(node, id, files[0].Name))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, f := range files {
		path := m.filePath(node, id, f.Name)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Size() == f.Size {
			continue
		}
		blob := filepath.Join(m.filesDir, "blobs", f.SHA256)
		if err := os.Link(blob, path); err == nil {
			continue
		}
		if err := copyFile(blob, path); err != nil {
			return err
		}
	}
	// Persist the links and the directories before acknowledging the message.
	for p := dir; ; p = filepath.Dir(p) {
		if err := syncDir(p); err != nil {
			return err
		}
		if p == m.filesDir {
			break
		}
	}
	return syncDir(filepath.Dir(m.filesDir))
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(dst), ".copy-")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(out.Name(), dst)
}

// cleanFiles removes blobs older than a day that no waiting outbox message
// needs. Received files live in in/ and are never removed here.
func (m *Mailbox) cleanFiles() error {
	// The transaction keeps sends from queuing a reference during deletion.
	return m.inTx(func(tx *sql.Tx) error {
		m.filesMu.Lock()
		defer m.filesMu.Unlock()
		rows, err := tx.Query(`SELECT envelope FROM outbox WHERE state = 'waiting'`)
		if err != nil {
			return err
		}
		defer rows.Close()
		keep := map[string]bool{}
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				return err
			}
			var env envelope.Envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				return err
			}
			for _, f := range env.Body.Files {
				keep[f.SHA256] = true
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		dir := filepath.Join(m.filesDir, "blobs")
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		cutoff := m.now().Add(-24 * time.Hour)
		for _, entry := range entries {
			// Uploads still being written have temporary names, not hashes.
			if len(entry.Name()) != 64 || keep[entry.Name()] {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode().IsRegular() && info.ModTime().Before(cutoff) {
				if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
