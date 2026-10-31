package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/DavidGuooo/baf/internal/mailbox"
)

func TestHumanSize(t *testing.T) {
	for _, tc := range []struct {
		size int64
		want string
	}{
		{0, "0 B"}, {512, "512 B"}, {1023, "1023 B"}, {1024, "1.0 KB"}, {1536, "1.5 KB"}, {1 << 20, "1.0 MB"}, {envelope.MaxFileSize, "100.0 MB"},
	} {
		if got := humanSize(tc.size); got != tc.want {
			t.Errorf("humanSize(%d) = %q, want %q", tc.size, got, tc.want)
		}
	}
}

func TestPrintItem(t *testing.T) {
	it := mailbox.Item{Message: envelope.Envelope{ID: "id", Kind: envelope.Result, ReplyTo: "request", FromThread: "sender-thread", ToThread: "receiver-thread", Body: envelope.Body{Text: "hello", State: envelope.Completed}}, From: "box/me", ReceivedAt: time.Date(2026, 10, 31, 12, 0, 0, 0, time.Local)}
	prefix := "id:          id\nkind:        result\nfrom:        box/me\nanswers:     request\nstate:       completed\nfrom_thread: sender-thread   (reply with --to-thread sender-thread)\nto_thread:   receiver-thread\nreceived:    2026-10-31 12:00:00\n"
	for _, withFiles := range []bool{false, true} {
		want := prefix
		if withFiles {
			it.Files = []mailbox.FileInfo{{File: envelope.File{Name: "report.pdf", Size: 1258291}, Path: "/tmp/report.pdf"}, {File: envelope.File{Name: "x.txt", Size: 512}, Path: "/tmp/x.txt"}}
			want += "file:        report.pdf (1.2 MB)  /tmp/report.pdf\nfile:        x.txt (512 B)  /tmp/x.txt\n"
		}
		var b bytes.Buffer
		printItem(&b, it)
		if got := b.String(); got != want+"\nhello\n" {
			t.Errorf("files=%v: got %q, want %q", withFiles, got, want+"\nhello\n")
		}
	}
}

func TestOpenFiles(t *testing.T) {
	dir := t.TempDir()
	makeFile := func(name string, size int64) string {
		t.Helper()
		path := filepath.Join(dir, name)
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(size); err != nil {
			t.Fatal(err)
		}
		f.Close()
		return path
	}
	a := makeFile("a.txt", 1)
	b := makeFile("b.txt", envelope.MaxFileSize)
	large := makeFile("large", envelope.MaxFileSize+1)
	badName := makeFile("bad\\name", 1)
	for _, tc := range []struct {
		name  string
		paths []string
		want  []string
	}{
		{"directory", []string{dir}, []string{"directory", "zip or tar"}},
		{"too many", make([]string, envelope.MaxFiles+1), []string{"at most 20"}},
		{"duplicate", []string{a, filepath.Join(dir, "A.TXT")}, []string{a, filepath.Join(dir, "A.TXT"), "ignoring case"}},
		{"bad name", []string{badName}, []string{"plain name"}},
		{"too large", []string{a, large}, []string{"limit is 100 MB"}},
		{"missing", []string{a, filepath.Join(dir, "missing")}, []string{"missing"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, err := openFiles(tc.paths)
			if err == nil {
				closeFiles(files)
				t.Fatal("expected error")
			}
			if files != nil {
				t.Error("returned files on error")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
		})
	}
	files, err := openFiles([]string{b, a})
	if err != nil {
		t.Fatal(err)
	}
	defer closeFiles(files)
	if len(files) != 2 || files[0].Name() != b || files[1].Name() != a {
		t.Fatalf("files lost input order: %v", files)
	}
}

func TestFileFlagsAndInboxText(t *testing.T) {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	var paths filePaths
	fs.Var(&paths, "file", "")
	pos, err := parse(fs, []string{"box/me", "--file", "a.pdf", "--file", "b.pdf"})
	if err != nil || len(pos) != 1 || len(paths) != 2 || paths[0] != "a.pdf" || paths[1] != "b.pdf" {
		t.Fatalf("pos=%v paths=%v err=%v", pos, paths, err)
	}
	it := mailbox.Item{Message: envelope.Envelope{Body: envelope.Body{Text: "hello\nworld"}}}
	for n, want := range []string{"hello", "[1 file] hello", "[2 files] hello"} {
		it.Message.Body.Files = make([]envelope.File, n)
		if got := inboxText(it); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}
