package mailbox

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DavidGuooo/baf/internal/agent"
	"github.com/DavidGuooo/baf/internal/envelope"
)

type fileTransport struct {
	Transport
	put    func(context.Context, envelope.File, io.Reader) error
	pushed bool
}

func (f *fileTransport) Known(context.Context, string) error { return nil }
func (f *fileTransport) PutFile(ctx context.Context, _, _ string, file envelope.File, body io.Reader) error {
	return f.put(ctx, file, body)
}
func (f *fileTransport) Push(context.Context, string, envelope.Envelope) (Receipt, error) {
	f.pushed = true
	return Receipt{}, nil
}

func TestFileHandoverFailureAndStopping(t *testing.T) {
	for _, mode := range []string{"missing", "retry", "stopping"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var opened *os.File
			tr := &fileTransport{put: func(ctx context.Context, f envelope.File, body io.Reader) error {
				opened = body.(*os.File)
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) <= pushTimeout {
					t.Error("file did not get extra time")
				}
				if mode == "stopping" {
					cancel()
					return ctx.Err()
				}
				return errors.New("connection lost")
			}}
			dir := t.TempDir()
			mb, err := Open(Options{Path: filepath.Join(dir, "mailbox.db"), Node: "grok", Agents: map[string]agent.Adapter{"tokdev": nil}, Transport: tr})
			if err != nil {
				t.Fatal(err)
			}
			defer mb.Close()
			f, err := mb.AddFile("report.pdf", strings.NewReader("report"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = mb.Send(envelope.Envelope{Kind: envelope.Notice, To: "imac/me", FromAgent: "tokdev", Body: envelope.Body{Text: "read", Files: []envelope.File{f}}})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "missing" {
				if err := os.Remove(filepath.Join(dir, "files", "blobs", f.SHA256)); err != nil {
					t.Fatal(err)
				}
			}
			heads, err := mb.outboxHeads()
			if err != nil || len(heads) != 1 {
				t.Fatalf("heads = %+v, err = %v", heads, err)
			}
			mb.handover(ctx, heads[0])
			if tr.pushed {
				t.Fatal("pushed a message without its file")
			}
			if opened != nil {
				if _, err := opened.Stat(); !errors.Is(err, os.ErrClosed) {
					t.Fatalf("file not closed: %v", err)
				}
			}
			out, err := mb.Outbox(true, 10)
			if err != nil || len(out) != 1 {
				t.Fatalf("outbox = %+v, err = %v", out, err)
			}
			notices, err := mb.Inbox("tokdev", true)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "missing":
				if out[0].State != "failed" || len(notices) != 1 || !strings.Contains(notices[0].Message.Body.Text, "report.pdf") || !strings.Contains(notices[0].Message.Body.Text, "no longer on this machine; send it again") {
					t.Fatalf("outbox = %+v, notices = %+v", out, notices)
				}
			case "retry":
				if out[0].State != "waiting" || out[0].Attempts != 1 || out[0].LastError != "handing over report.pdf: connection lost" || len(notices) != 0 || !out[0].NextTry.After(heads[0].nextTry) {
					t.Fatalf("outbox = %+v, notices = %+v", out, notices)
				}
			case "stopping":
				if out[0].State != "waiting" || out[0].Attempts != 0 || out[0].LastError != "" || len(notices) != 0 {
					t.Fatalf("outbox = %+v, notices = %+v", out, notices)
				}
			}
		})
	}
}
