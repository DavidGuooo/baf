package main

import (
	"strings"
	"testing"
	"time"

	"github.com/DavidGuooo/baf/internal/mailbox"
)

func TestPrintStatus(t *testing.T) {
	t0 := time.Date(2026, 10, 31, 14, 2, 10, 0, time.Local)
	at := func(sec int) *time.Time { v := t0.Add(time.Duration(sec) * time.Second); return &v }
	running := mailbox.SentStatus{
		ID: "01REQ", Kind: "request", FromAgent: "me", To: "grokbot/tokdev", SentAt: t0,
		Handover: &mailbox.OutItem{State: "handed", Attempts: 1, DoneAt: at(1)},
		Receiver: &mailbox.Progress{State: "running", Run: &mailbox.RunProgress{
			State: "running", StartedAt: *at(2), Doing: "running: go test ./...", DoingAt: at(100)}},
	}
	answered := mailbox.SentStatus{
		ID: "01REQ", Kind: "request", FromAgent: "me", To: "grokbot/tokdev", SentAt: t0,
		Handover: &mailbox.OutItem{State: "handed", Attempts: 3, DoneAt: at(1)},
		Result:   &mailbox.ResultInfo{ID: "01RES", State: "completed", ReceivedAt: *at(200)},
	}
	unreachable := mailbox.SentStatus{
		ID: "01N", Kind: "notice", FromAgent: "me", To: "grokbot/tokdev", SentAt: t0,
		Handover:      &mailbox.OutItem{State: "handed", Attempts: 1, DoneAt: at(1)},
		ReceiverError: "connect: host is down",
	}
	inbox := mailbox.SentStatus{
		ID: "01N", Kind: "notice", FromAgent: "codex", To: "imac/me", SentAt: t0,
		Receiver: &mailbox.Progress{To: "imac/me", State: "inbox"},
	}
	for name, c := range map[string]struct {
		s    mailbox.SentStatus
		want []string
	}{
		"running": {running, []string{
			"request 01REQ from me to grokbot/tokdev",
			"handed over  Oct 31 14:02:11 (1 try)",
			"run          running for 2m28s",
			"doing        running: go test ./... (started 50s ago)",
			"result       not yet",
		}},
		"answered":    {answered, []string{"(3 tries)", "result       completed, Oct 31 14:05:30 (read it: baf read 01RES)"}},
		"unreachable": {unreachable, []string{"receiver     could not ask grokbot: connect: host is down"}},
		"inbox":       {inbox, []string{"receiver     in me's inbox; not opened yet"}},
	} {
		var b strings.Builder
		printStatus(&b, c.s, t0.Add(150*time.Second))
		for _, w := range c.want {
			if !strings.Contains(b.String(), w) {
				t.Errorf("%s: missing %q in:\n%s", name, w, b.String())
			}
		}
		if name != "running" && name != "answered" && strings.Contains(b.String(), "result") {
			t.Errorf("%s: a notice gets no result line:\n%s", name, b.String())
		}
	}
}
