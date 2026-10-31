package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/DavidGuooo/baf/internal/mailbox"
)

// filePaths collects every --file given.
type filePaths []string

func (p *filePaths) String() string { return strings.Join(*p, ", ") }
func (p *filePaths) Set(path string) error {
	*p = append(*p, path)
	return nil
}

// openFiles checks and opens every file before any is uploaded, so a bad
// path fails the send before anything reaches the mailbox.
func openFiles(paths []string) (files []*os.File, err error) {
	defer func() {
		if err != nil {
			closeFiles(files)
			files = nil
		}
	}()
	if len(paths) > envelope.MaxFiles {
		return nil, fmt.Errorf("%d files; a message may carry at most %d", len(paths), envelope.MaxFiles)
	}
	seen := map[string]string{}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return files, err
		}
		if info.IsDir() {
			return files, fmt.Errorf("%q is a directory; zip or tar the folder first", path)
		}
		name := filepath.Base(path)
		if err := envelope.CheckFileName(name); err != nil {
			return files, fmt.Errorf("file %q: %w", path, err)
		}
		key := strings.ToLower(name)
		if previous, ok := seen[key]; ok {
			return files, fmt.Errorf("files %q and %q have the same name ignoring case", previous, path)
		}
		seen[key] = path
		if !info.Mode().IsRegular() {
			return files, fmt.Errorf("%q is not a regular file", path)
		}
		f, err := os.Open(path)
		if err != nil {
			return files, err
		}
		files = append(files, f)
		info, err = f.Stat()
		if err != nil {
			return files, err
		}
		if !info.Mode().IsRegular() {
			return files, fmt.Errorf("%q is not a regular file", path)
		}
		if info.Size() > envelope.MaxFileSize {
			return files, fmt.Errorf("file %q is %s; the limit is %d MB", path, humanSize(info.Size()), envelope.MaxFileSize>>20)
		}
	}
	return files, nil
}

func closeFiles(files []*os.File) {
	for _, f := range files {
		f.Close()
	}
}

// humanSize shows a file size the way people read it: 512 B, 1.2 MB.
func humanSize(size int64) string {
	switch {
	case size < 1024:
		return fmt.Sprintf("%d B", size)
	case size < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(size)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(size)/(1024*1024))
	}
}

func fileCount(n int) string {
	if n == 1 {
		return "1 file"
	}
	return fmt.Sprintf("%d files", n)
}

// inboxText is the TEXT column of baf inbox: the first line, after a file count.
func inboxText(it mailbox.Item) string {
	text := firstLine(it.Message.Body.Text, 60)
	if n := len(it.Message.Body.Files); n > 0 {
		text = "[" + fileCount(n) + "] " + text
	}
	return text
}
