package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DavidGuooo/baf/internal/agent"
	"github.com/DavidGuooo/baf/internal/daemon"
	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/DavidGuooo/baf/internal/mailbox"
)

func TestInboxCommandAndWait(t *testing.T) {
	// Keep the Unix socket path short enough for macOS.
	home, err := os.MkdirTemp("/tmp", "baf-inbox-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	t.Setenv("BAF_HOME", home)
	t.Setenv("BAF_AGENT", "me")
	mb, err := mailbox.Open(mailbox.Options{Path: filepath.Join(home, "mailbox.db"), Node: "box", Agents: map[string]agent.Adapter{"me": nil, "codex": nil}})
	if err != nil {
		t.Fatal(err)
	}
	defer mb.Close()
	ln, err := net.Listen("unix", daemon.SocketPath(home))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(daemon.Handler(mb, []string{"me", "codex"}))
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	defer srv.Close()
	for i := 0; i < 60; i++ {
		if _, err := mb.Send(envelope.Envelope{ID: fmt.Sprintf("mail-%02d", i), Kind: envelope.Notice, FromAgent: "codex", To: "box/me"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"mail-00", "mail-59"} {
		if _, err := mb.Read("me", id); err != nil {
			t.Fatal(err)
		}
	}
	capture := func(args ...string) []byte {
		t.Helper()
		f, err := os.CreateTemp(home, "stdout-")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		old := os.Stdout
		os.Stdout = f
		defer func() { os.Stdout = old }()
		if err := inbox(args); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	for _, tc := range []struct {
		args               []string
		count, first, step int
	}{
		{[]string{"--json"}, 50, 59, -1},
		{[]string{"--as", "codex", "--json"}, 0, 0, 0},
	} {
		var items []mailbox.Item
		if err := json.Unmarshal(capture(tc.args...), &items); err != nil {
			t.Fatal(err)
		}
		if len(items) != tc.count {
			t.Fatalf("%v: %d items, want %d", tc.args, len(items), tc.count)
		}
		for i, item := range items {
			n := tc.first + i*tc.step
			if item.Message.ID != fmt.Sprintf("mail-%02d", n) || item.Read != (n == 59) {
				t.Fatalf("%v: item %d = %+v", tc.args, i, item)
			}
		}
	}
	lines := strings.Split(strings.TrimSpace(string(capture())), "\n")
	if len(lines) != 51 {
		t.Fatalf("table has %d lines, want 51", len(lines))
	}
	for i, want := range []string{"READ", "yes", "no"} {
		fields := strings.Fields(lines[i])
		if len(fields) < 5 || fields[4] != want {
			t.Fatalf("table line %d = %q, want read column %q", i, lines[i], want)
		}
	}
	// Both a read and an unread message older than the latest 50 must still be found.
	if err := wait([]string{"--as", "me", "--timeout", "0s", "mail-00", "mail-01"}); err != nil {
		t.Fatal(err)
	}
	if err := wait([]string{"--as", "codex", "--timeout", "0s", "mail-00"}); err == nil {
		t.Fatal("wait found another agent's mail")
	}
}
