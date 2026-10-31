package main

import (
	"context"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/DavidGuooo/baf/internal/agent"
	"github.com/DavidGuooo/baf/internal/daemon"
	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/DavidGuooo/baf/internal/mailbox"
	"github.com/DavidGuooo/baf/internal/tailnet"
)

type commandDoorIdentity struct{}

func (commandDoorIdentity) SelfPeer(context.Context) (tailnet.Peer, error) {
	return tailnet.Peer{Grants: []tailnet.Grant{{Relay: true, Door: map[string][]string{"openai/dot": {"codex", "me"}}}}}, nil
}

func TestSendCommandToDoorAgent(t *testing.T) {
	// A short directory keeps the socket within macOS's Unix path limit.
	home, err := os.MkdirTemp("/tmp", "baf-door-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	t.Setenv("BAF_HOME", home)
	t.Setenv("BAF_AGENT", "codex")
	mb, err := mailbox.Open(mailbox.Options{
		Path: filepath.Join(home, "mailbox.db"), Node: "box",
		Agents: map[string]agent.Adapter{"codex": nil, "me": nil}, Door: commandDoorIdentity{},
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
	if err := send([]string{"openai/dot", "hello from Codex"}, envelope.Notice); err != nil {
		t.Fatal(err)
	}
	items, err := mb.DoorInbox("openai/dot", false)
	if err != nil || len(items) != 1 || items[0].From != "box/codex" || items[0].Message.Body.Text != "hello from Codex" {
		t.Fatalf("inbox = %+v, err = %v", items, err)
	}
	c := daemon.NewClient(daemon.SocketPath(home))
	s, err := c.SentStatus(items[0].Message.ID)
	if err != nil || s.Handover != nil || s.Receiver == nil || s.Receiver.To != "openai/dot" {
		t.Fatalf("status = %+v, err = %v", s, err)
	}
}
