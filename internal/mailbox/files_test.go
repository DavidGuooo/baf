package mailbox

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DavidGuooo/baf/internal/agent"
	"github.com/DavidGuooo/baf/internal/envelope"
)

func addedFile(t *testing.T, m *Mailbox, name, text string) envelope.File {
	t.Helper()
	f, err := m.AddFile(name, strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	if f.Name != name || f.Size != int64(len(text)) || f.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(text))) {
		t.Fatalf("file = %+v", f)
	}
	return f
}

func fileBytes(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file permissions = %o", info.Mode().Perm())
	}
}

func fileRejection(t *testing.T, err error, part string) {
	t.Helper()
	var rej *Rejection
	if !errors.As(err, &rej) || !strings.Contains(err.Error(), part) {
		t.Fatalf("error = %v, want rejection containing %q", err, part)
	}
}

func TestFilesReachWorkerAndInbox(t *testing.T) {
	h := newHarness(t)
	f := addedFile(t, h.mb, "report.pdf", "report bytes")
	if _, err := h.send(envelope.Envelope{Kind: envelope.Request, Body: envelope.Body{Files: []envelope.File{f}}}); err != nil {
		t.Fatal(err)
	}
	h.waitInbox("me", 1)
	h.worker.mu.Lock()
	job := h.worker.jobs[0]
	h.worker.mu.Unlock()
	if len(job.Files) != 1 || job.Files[0].Name != f.Name || job.Files[0].Size != f.Size {
		t.Fatalf("job files = %+v", job.Files)
	}
	fileBytes(t, job.Files[0].Path, "report bytes")
	if !filepath.IsAbs(job.Files[0].Path) || !strings.Contains(agent.Prompt(job), "- report.pdf (12 bytes): "+job.Files[0].Path) {
		t.Fatalf("prompt = %s", agent.Prompt(job))
	}
	r, err := h.send(envelope.Envelope{Kind: envelope.Notice, To: "box/other", Body: envelope.Body{Files: []envelope.File{f}}})
	if err != nil {
		t.Fatal(err)
	}
	item := h.waitInbox("other", 1)[0]
	read, err := h.mb.Read("other", r.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range []Item{item, read} {
		if len(it.Files) != 1 || it.Files[0].File != f {
			t.Fatalf("files = %+v", it.Files)
		}
		fileBytes(t, it.Files[0].Path, "report bytes")
		for dir := filepath.Dir(it.Files[0].Path); dir != filepath.Dir(h.mb.filesDir); dir = filepath.Dir(dir) {
			info, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o700 {
				t.Errorf("%s permissions = %o", dir, info.Mode().Perm())
			}
		}
	}
}

func TestFilesMustArriveBeforeMessage(t *testing.T) {
	h := newHarness(t)
	text := "from far"
	f := envelope.File{Name: "far.txt", Size: int64(len(text)), SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(text)))}
	env := envelope.Envelope{V: 1, ID: envelope.NewID(), Kind: envelope.Notice, FromAgent: "me", To: "box/other", Body: envelope.Body{Files: []envelope.File{f}}}
	_, err := h.mb.Send(env)
	fileRejection(t, err, "--file")
	from := Principal{Node: "far", Agent: "me"}
	_, err = h.mb.Accept(env, from)
	fileRejection(t, err, "wasn't handed over")
	if err := h.mb.StoreFile(f, strings.NewReader(text)); err != nil {
		t.Fatal(err)
	}
	if err := h.mb.StoreFile(f, strings.NewReader(text)); err != nil {
		t.Fatal(err)
	}
	r, err := h.mb.Accept(env, from)
	if err != nil || r.Duplicate {
		t.Fatalf("accept = %+v, %v", r, err)
	}
	path := h.mb.filePath("far", env.ID, f.Name)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Duplicates must not need the blob or replace the received file.
	if err := os.Remove(filepath.Join(h.mb.filesDir, "blobs", f.SHA256)); err != nil {
		t.Fatal(err)
	}
	r, err = h.mb.Accept(env, from)
	if err != nil || !r.Duplicate {
		t.Fatalf("retry = %+v, %v", r, err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("retry replaced file: %v", err)
	}
	fileBytes(t, path, text)
}

type endlessFile struct{ n int64 }

func (r *endlessFile) Read(p []byte) (int, error) {
	clear(p)
	r.n += int64(len(p))
	return len(p), nil
}

type brokenFile struct{}

func (brokenFile) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestFileUploadsRejectAndRemoveTemps(t *testing.T) {
	h := newHarness(t)
	f := envelope.File{Name: "a.txt", Size: 3, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("abc")))}
	for _, tc := range []struct {
		name, reason string
		file         envelope.File
		reader       io.Reader
	}{
		{"hash", "sha256 mismatch", f, strings.NewReader("abd")},
		{"size", "size mismatch", f, strings.NewReader("ab")},
		{"limit", "exceeds the limit", f, &endlessFile{}},
		{"declared limit", "outside the limit", envelope.File{Name: "a", Size: envelope.MaxFileSize + 1}, strings.NewReader("")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fileRejection(t, h.mb.StoreFile(tc.file, tc.reader), tc.reason)
			if r, ok := tc.reader.(*endlessFile); ok && r.n != envelope.MaxFileSize+1 {
				t.Fatalf("read %d bytes", r.n)
			}
		})
	}
	for _, name := range []string{"", "..", "../bad", "bad\\name", "bad\nname"} {
		_, err := h.mb.AddFile(name, strings.NewReader("x"))
		fileRejection(t, err, "name")
	}
	r := &endlessFile{}
	_, err := h.mb.AddFile("big", r)
	fileRejection(t, err, "exceeds the limit")
	if r.n != envelope.MaxFileSize+1 {
		t.Fatalf("read %d bytes", r.n)
	}
	if _, err := h.mb.AddFile("broken", brokenFile{}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read error = %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(h.mb.filesDir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed uploads left %v", entries)
	}
}

func TestCleanFilesKeepsWaitingAndReceived(t *testing.T) {
	h := newHarness(t)
	old := addedFile(t, h.mb, "old", "old")
	fresh := addedFile(t, h.mb, "fresh", "fresh")
	waiting := addedFile(t, h.mb, "waiting", "waiting")
	r, err := h.send(envelope.Envelope{Kind: envelope.Notice, To: "box/other", Body: envelope.Body{Files: []envelope.File{old}}})
	if err != nil {
		t.Fatal(err)
	}
	env := envelope.Envelope{V: 1, ID: envelope.NewID(), Kind: envelope.Notice, FromAgent: "me", To: "far/me", Body: envelope.Body{Files: []envelope.File{waiting}}}
	err = h.mb.inTx(func(tx *sql.Tx) error {
		_, err := h.mb.enqueueTx(tx, env, Principal{Node: "far", Agent: "me"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []envelope.File{old, waiting} {
		then := h.clock.Add(-25 * time.Hour)
		if err := os.Chtimes(filepath.Join(h.mb.filesDir, "blobs", f.SHA256), then, then); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.mb.cleanFiles(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.mb.openFile(old.SHA256); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old blob: %v", err)
	}
	for _, f := range []envelope.File{fresh, waiting} {
		fileBytes(t, filepath.Join(h.mb.filesDir, "blobs", f.SHA256), f.Name)
	}
	fileBytes(t, h.mb.filePath("box", r.ID, old.Name), "old")
	// Startup performs the same cleanup.
	then := h.clock.Add(-25 * time.Hour)
	if err := os.Chtimes(filepath.Join(h.mb.filesDir, "blobs", fresh.SHA256), then, then); err != nil {
		t.Fatal(err)
	}
	h.stop()
	h.open()
	if _, err := h.mb.openFile(fresh.SHA256); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("startup kept old blob: %v", err)
	}
	fileBytes(t, h.mb.filePath("box", r.ID, old.Name), "old")
}

func TestPlaceFilesReusesRetryAndRejectsUnsafeID(t *testing.T) {
	h := newHarness(t)
	f := addedFile(t, h.mb, "file", "bytes")
	id := envelope.NewID()
	if err := h.mb.placeFiles("far", id, []envelope.File{f}); err != nil {
		t.Fatal(err)
	}
	path := h.mb.filePath("far", id, f.Name)
	before, _ := os.Stat(path)
	if err := h.mb.placeFiles("far", id, []envelope.File{f}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !os.SameFile(before, after) {
		t.Fatal("retry replaced file")
	}
	fileRejection(t, h.mb.placeFiles("far", "../../escape", []envelope.File{f}), "bad message id")
	// An incomplete earlier copy forces the link attempt to fall back to copying.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.mb.placeFiles("far", id, []envelope.File{f}); err != nil {
		t.Fatal(err)
	}
	fileBytes(t, path, "bytes")
	// Exercise the copy path independently of filesystem hard-link support.
	copied := filepath.Join(t.TempDir(), "copy")
	if err := copyFile(path, copied); err != nil {
		t.Fatal(err)
	}
	fileBytes(t, copied, "bytes")
}

func TestAddFileReusesBlob(t *testing.T) {
	h := newHarness(t)
	f := addedFile(t, h.mb, "one", "same")
	path := filepath.Join(h.mb.filesDir, "blobs", f.SHA256)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	old := h.clock.Add(-25 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	again := addedFile(t, h.mb, "two", "same")
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.SHA256 != f.SHA256 || !os.SameFile(before, after) {
		t.Fatal("replaced existing blob")
	}
	if err := h.mb.cleanFiles(); err != nil {
		t.Fatal(err)
	}
	fileBytes(t, path, "same")
	empty := addedFile(t, h.mb, "empty", "")
	fileBytes(t, filepath.Join(h.mb.filesDir, "blobs", empty.SHA256), "")
	entries, err := os.ReadDir(filepath.Join(h.mb.filesDir, "blobs"))
	if err != nil || len(entries) != 2 {
		t.Fatalf("blobs = %v, %v", entries, err)
	}
}
