package tailnet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"tailscale.com/client/local"
)

func TestDoorGrants(t *testing.T) {
	var p Peer
	if err := json.Unmarshal([]byte(`{"Grants":[
		{"to":["*"],"relay":true,"door":{"openai/dot":["codex","me"],"openai/quiet":[],"bad/name/extra":["*"]}},
		{"relay":true,"door":{"openai/dot":["claude"],"anthropic/claude":["*"]}},
		{"door":{"untrusted/dot":["*"]}}
	]}`), &p); err != nil {
		t.Fatal(err)
	}
	if names := p.DoorAgents(); !slices.Equal(names, []string{"anthropic/claude", "openai/dot", "openai/quiet"}) {
		t.Fatalf("names = %v", names)
	}
	for _, tc := range []struct {
		door, agent string
		want        bool
	}{
		{"openai/dot", "codex", true}, {"openai/dot", "me", true}, {"openai/dot", "claude", true},
		{"openai/dot", "other", false}, {"openai/quiet", "me", false}, {"missing/dot", "me", false},
		{"anthropic/claude", "anything", true}, {"untrusted/dot", "me", false}, {"bad/name/extra", "me", false},
	} {
		if got := p.DoorMayMessage(tc.door, tc.agent); got != tc.want {
			t.Errorf("%s -> %s = %v, want %v", tc.door, tc.agent, got, tc.want)
		}
	}
	if !p.MayMessage("other") {
		t.Fatal("ordinary To grant changed")
	}
}

func TestMalformedDoorNamesLoggedOnce(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	bad := []string{"", "dot", "/dot", "openai/", "OpenAI/dot", "openai/do.t", "openai/dot/extra", "../dot", "openai/*"}
	g := Grant{Relay: true, Door: map[string][]string{"vendor-1/agent_2": {"me"}}}
	for _, name := range bad {
		malformedDoors.Delete(name)
		g.Door[name] = []string{"*"}
	}
	p := Peer{Grants: []Grant{g}}
	for range 3 {
		if names := p.DoorAgents(); !slices.Equal(names, []string{"vendor-1/agent_2"}) {
			t.Fatalf("names = %v", names)
		}
	}
	if n := strings.Count(logs.String(), "ignoring malformed Door agent"); n != len(bad) {
		t.Fatalf("logged %d times, want %d: %s", n, len(bad), logs.String())
	}
}

func TestSelfPeerReadsFreshWhoIsOnOwnAddress(t *testing.T) {
	var mu sync.Mutex
	listed := true
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/localapi/v0/status":
			fmt.Fprint(w, `{"Self":{"DNSName":"imac.example.ts.net."},"TailscaleIPs":["100.64.0.1"]}`)
		case "/localapi/v0/whois":
			if addr := r.URL.Query().Get("addr"); addr != "100.64.0.1:0" {
				t.Errorf("WhoIs address = %q", addr)
			}
			mu.Lock()
			defer mu.Unlock()
			calls++
			door := map[string][]string{}
			if listed {
				door["openai/dot"] = []string{"codex", "me"}
			}
			json.NewEncoder(w).Encode(map[string]any{
				"Node":   map[string]string{"Name": "imac.example.ts.net."},
				"CapMap": map[string]any{Capability: []Grant{{Relay: true, Door: door}}},
			})
		default:
			t.Errorf("unexpected LocalAPI path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &Client{lc: local.Client{OmitAuth: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", srv.Listener.Addr().String())
	}}}
	p, err := c.SelfPeer(context.Background())
	if err != nil || p.Node != "imac" || !p.DoorMayMessage("openai/dot", "codex") {
		t.Fatalf("peer = %+v, err = %v", p, err)
	}
	mu.Lock()
	listed = false
	mu.Unlock()
	p, err = c.SelfPeer(context.Background())
	if err != nil || len(p.DoorAgents()) != 0 {
		t.Fatalf("stale grants = %+v, err = %v", p, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("WhoIs called %d times", calls)
	}
}
