package mailbox

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DavidGuooo/baf/internal/agent"
	"github.com/DavidGuooo/baf/internal/envelope"
)

// waitStatus polls the status of message id until done says it is enough.
func (h *harness) waitStatus(id string, done func(SentStatus) bool) SentStatus {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s, err := h.mb.SentStatus(context.Background(), id)
		if err != nil {
			h.t.Fatal(err)
		}
		if done(s) {
			return s
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("status of %s stayed %+v", id, s)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func running(s SentStatus) bool {
	return s.Receiver != nil && s.Receiver.Run != nil && s.Receiver.Run.Doing != ""
}

func TestStatusFollowsALocalRequest(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	h.worker.doing = func(r agent.Report) { r.Doing("running: go test ./...") }
	h.worker.reply = func(ctx context.Context, job agent.Job) agent.Outcome {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return agent.Outcome{State: envelope.Completed, Text: "ok"}
	}
	r, err := h.send(envelope.Envelope{Kind: envelope.Request, Body: envelope.Body{Text: "test it"}})
	if err != nil {
		t.Fatal(err)
	}

	s := h.waitStatus(r.ID, running)
	if s.Kind != "request" || s.FromAgent != "me" || s.To != "box/worker" || s.Handover != nil || s.Result != nil {
		t.Fatalf("status = %+v", s)
	}
	p := s.Receiver
	if p.State != "running" || p.Run.State != "running" || p.Run.ThreadID != "t1" || p.Run.EndedAt != nil ||
		p.Run.Doing != "running: go test ./..." || p.Run.DoingAt == nil || p.ReadAt == nil {
		t.Fatalf("progress = %+v, run = %+v", p, p.Run)
	}

	close(release)
	s = h.waitStatus(r.ID, func(s SentStatus) bool { return s.Result != nil })
	if s.Result.State != "completed" || s.Receiver.State != "done" || s.Receiver.Run.State != "completed" ||
		s.Receiver.Run.EndedAt == nil || s.Receiver.Run.Doing != "running: go test ./..." {
		t.Fatalf("status = %+v, run = %+v", s, s.Receiver.Run)
	}
}

func TestStatusOfAnInboxMessage(t *testing.T) {
	h := newHarness(t)
	r, err := h.send(envelope.Envelope{Kind: envelope.Notice, To: "box/other", Body: envelope.Body{Text: "fyi"}})
	if err != nil {
		t.Fatal(err)
	}
	s := h.waitStatus(r.ID, func(SentStatus) bool { return true })
	if s.Receiver.State != "inbox" || s.Receiver.ReadAt != nil || s.Receiver.Run != nil || s.Result != nil {
		t.Fatalf("status = %+v", s)
	}
	if _, err := h.mb.Read("other", r.ID); err != nil {
		t.Fatal(err)
	}
	if s := h.waitStatus(r.ID, func(SentStatus) bool { return true }); s.Receiver.ReadAt == nil {
		t.Fatal("reading it should show when it was opened")
	}
}

func TestProgressOnlyForTheSendingNode(t *testing.T) {
	h := newHarness(t)
	r, err := h.send(envelope.Envelope{Kind: envelope.Notice, To: "box/other", Body: envelope.Body{Text: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.mb.Progress("box", r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.mb.Progress("elsewhere", r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another node asking: err = %v, want ErrNotFound", err)
	}
	if _, err := h.mb.SentStatus(context.Background(), "01NOSUCH"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: err = %v", err)
	}
}

// A mailbox.db from before progress existed gets its new columns on open.
func TestOlderDatabaseGetsNewColumns(t *testing.T) {
	h := newHarness(t)
	h.stop()
	db, err := sql.Open("sqlite", "file:"+h.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`ALTER TABLE messages DROP COLUMN notified_at`, `ALTER TABLE runs DROP COLUMN doing`, `ALTER TABLE runs DROP COLUMN doing_at`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	h.open()
	h.worker.doing = func(r agent.Report) { r.Doing("reading main.go") }
	r, err := h.send(envelope.Envelope{Kind: envelope.Request, Body: envelope.Body{Text: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	s := h.waitStatus(r.ID, func(s SentStatus) bool { return s.Result != nil })
	if s.Receiver.Run.Doing != "reading main.go" {
		t.Fatalf("run = %+v", s.Receiver.Run)
	}
}
