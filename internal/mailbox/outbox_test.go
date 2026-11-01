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
	put     func(context.Context, envelope.File, io.Reader) error
	pushed  bool
	pushErr error
}

func (f *fileTransport) Known(context.Context, string) error { return nil }
func (f *fileTransport) PutFile(ctx context.Context, _, _, _ string, file envelope.File, body io.Reader) error {
	return f.put(ctx, file, body)
}
func (f *fileTransport) Push(context.Context, string, envelope.Envelope) (Receipt, error) {
	f.pushed = true
	return Receipt{}, f.pushErr
}

type doorbellFunc func(context.Context, string, envelope.Envelope) error

func (f doorbellFunc) Ring(ctx context.Context, node string, env envelope.Envelope) error {
	return f(ctx, node, env)
}

func TestHandoverWaitsForDoorbell(t *testing.T) {
	for _, mode := range []string{"ring fails", "push fails", "no doorbell", "no doorbells configured"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now().Truncate(time.Millisecond)
			tr := &fileTransport{}
			rings := 0
			var bell Doorbell = doorbellFunc(func(_ context.Context, node string, env envelope.Envelope) error {
				rings++
				if node != "grokbot" || env.ID != "doorbell-test" || tr.pushed {
					t.Fatalf("ring after push or wrong message: node=%s env=%+v pushed=%v", node, env, tr.pushed)
				}
				if mode == "ring fails" && rings == 1 {
					return errors.New("Bridge unavailable")
				}
				if mode == "no doorbell" {
					return ErrNoDoorbell
				}
				return nil
			})
			if mode == "no doorbells configured" {
				bell = nil
			}
			if mode == "push fails" {
				tr.pushErr = errors.New("node offline")
			}
			mb, err := Open(Options{
				Path: filepath.Join(t.TempDir(), "mailbox.db"), Node: "imac",
				Agents: map[string]agent.Adapter{"me": nil}, Transport: tr, Doorbell: bell,
				Now: func() time.Time { return now }, RetryDelay: 5 * time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer mb.Close()
			if _, err := mb.Send(envelope.Envelope{ID: "doorbell-test", Kind: envelope.Notice, To: "grokbot/tokdev", FromAgent: "me"}); err != nil {
				t.Fatal(err)
			}
			attempt := func() {
				t.Helper()
				heads, err := mb.outboxHeads()
				if err != nil || len(heads) != 1 {
					t.Fatalf("heads = %+v, err = %v", heads, err)
				}
				mb.handover(context.Background(), heads[0])
			}
			attempt()
			wantAttempts, wantRings := 1, 1
			if mode == "ring fails" || mode == "push fails" {
				out, err := mb.Outbox(true, 10)
				wantError := "node offline"
				if mode == "ring fails" {
					wantError = "doorbell did not ring: Bridge unavailable"
				}
				if err != nil || len(out) != 1 || out[0].State != "waiting" || out[0].Attempts != 1 ||
					out[0].LastError != wantError || !out[0].NextTry.Equal(now.Add(5*time.Second)) ||
					!out[0].ExpiresAt.Equal(now.Add(24*time.Hour)) {
					t.Fatalf("outbox = %+v, err = %v", out, err)
				}
				heads, err := mb.outboxHeads()
				if err != nil || len(heads) != 1 || heads[0].rung != (mode == "push fails") {
					t.Fatalf("heads = %+v, err = %v", heads, err)
				}
				if rings != 1 || tr.pushed != (mode == "push fails") {
					t.Fatalf("rings=%d pushed=%v", rings, tr.pushed)
				}
				now = out[0].NextTry
				tr.pushErr = nil
				attempt()
				wantAttempts = 2
				if mode == "ring fails" {
					wantRings = 2
				}
			}
			if mode == "no doorbells configured" {
				wantRings = 0
			}
			out, err := mb.Outbox(true, 10)
			if err != nil || len(out) != 1 || out[0].State != "handed" || out[0].LastError != "" || out[0].Attempts != wantAttempts {
				t.Fatalf("outbox = %+v, err = %v", out, err)
			}
			var rung bool
			if err := mb.db.QueryRow(`SELECT rung FROM outbox WHERE id = ?`, "doorbell-test").Scan(&rung); err != nil {
				t.Fatal(err)
			}
			if rings != wantRings || !tr.pushed || rung != (bell != nil) {
				t.Fatalf("rings=%d pushed=%v rung=%v", rings, tr.pushed, rung)
			}
		})
	}
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
