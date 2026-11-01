package main

import (
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/DavidGuooo/baf/internal/agent"
	"github.com/DavidGuooo/baf/internal/daemon"
	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/DavidGuooo/baf/internal/mailbox"
)

func TestWaitingIDs(t *testing.T) {
	items := []mailbox.OutItem{
		{ID: "01A", FromAgent: "tokdev", State: "waiting"},
		{ID: "01B", FromAgent: "bridge", State: "waiting"},
		{ID: "01C", FromAgent: "tokdev", State: "handed"},
		{ID: "01D", FromAgent: "tokdev", State: "failed"},
		{ID: "01E", FromAgent: "tokdev", State: "waiting"},
		{ID: "01F", FromAgent: "dot", Origin: "openai/dot", State: "waiting"},
	}
	if got := waitingIDs(items); !slices.Equal(got, []string{"01A", "01B", "01E", "01F"}) {
		t.Errorf("got %v, want every agent's waiting messages", got)
	}
	if got := waitingIDs(items[2:4]); len(got) != 0 {
		t.Errorf("got %v, want no handed or failed messages", got)
	}
}

func TestFlushAllAgents(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "baf-flush-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	t.Setenv("BAF_HOME", home)
	t.Setenv("BAF_AGENT", "me")
	mb, err := mailbox.Open(mailbox.Options{
		Path: filepath.Join(home, "mailbox.db"), Node: "box", Door: commandDoorIdentity{},
		Agents: map[string]agent.Adapter{"codex": nil, "me": nil}, Transport: commandOutboxTransport{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mb.Close()
	ln, err := net.Listen("unix", daemon.SocketPath(home))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(daemon.Handler(mb, []string{"codex", "me"}))
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	defer srv.Close()
	if err := flush([]string{"--timeout", "0s"}); err != nil {
		t.Fatalf("empty outbox: %v", err)
	}
	if _, err := mb.Send(envelope.Envelope{ID: "codex-mail", Kind: envelope.Notice, FromAgent: "codex", To: "other/me"}); err != nil {
		t.Fatal(err)
	}
	if _, err := mb.FromDoor("openai/dot", envelope.Envelope{ID: "dot-mail", Kind: envelope.Notice, To: "other/me"}); err != nil {
		t.Fatal(err)
	}
	err = flush([]string{"--timeout", "0s"})
	if err == nil || !strings.Contains(err.Error(), "still waiting after 0s:") ||
		!strings.Contains(err.Error(), "codex-mail") || !strings.Contains(err.Error(), "dot-mail") {
		t.Fatalf("flush with other agents' mail waiting: %v", err)
	}
}
