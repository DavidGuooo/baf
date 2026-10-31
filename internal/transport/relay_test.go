package transport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DavidGuooo/baf/internal/agent"
	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/DavidGuooo/baf/internal/mailbox"
	"github.com/DavidGuooo/baf/internal/tailnet"
)

type relayPolicy struct{}

func (relayPolicy) SelfPeer(context.Context) (tailnet.Peer, error) {
	return tailnet.Peer{Node: "imac", Grants: []tailnet.Grant{{Relay: true, Door: map[string][]string{
		"openai/dot": {"me", "openai/other"}, "openai/other": {}, "anthropic/dot": {},
	}}}}, nil
}
func relayGrant() tailnet.Grant {
	return tailnet.Grant{Relay: true, Door: map[string][]string{"openai/dot": {"tokdev"}}}
}
func findIMac(context.Context) (string, error) { return "imac", nil }

type eventWake struct{ calls atomic.Int64 }

func (e *eventWake) Wake() { e.calls.Add(1) }

func TestDoorAcrossRelay(t *testing.T) {
	ctx := context.Background()
	rings := make(chan ringBody, 10)
	bell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body ringBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		rings <- body
		w.WriteHeader(200)
	}))
	defer bell.Close()
	n := newNet(t)
	relay := n.add("imac", "100.64.0.1", nodeOpts{agents: map[string]agent.Adapter{"me": nil}, door: relayPolicy{},
		doorbell: &Doorbells{From: "imac", Bells: map[string]Bell{"grokbot": {URL: bell.URL}}}})
	grok := n.add("grokbot", "100.64.0.2", nodeOpts{agents: map[string]agent.Adapter{"*": nil}, findRelay: findIMac})
	plain := n.add("plain", "100.64.0.3", nodeOpts{agents: map[string]agent.Adapter{"me": nil}})
	_ = plain
	relay.callerIs("grokbot", tailnet.Grant{To: []string{"openai/dot"}})
	grok.callerIs("imac", relayGrant(), tailnet.Grant{To: []string{"*"}})
	wake := &eventWake{}
	relay.mb.SetDoorEvents(wake)

	req, err := relay.mb.FromDoor("openai/dot", envelope.Envelope{ID: "request", Kind: envelope.Request, To: "grokbot/tokdev", FromThread: "dot-thread"})
	if err != nil || !req.Queued {
		t.Fatalf("send = %+v, %v", req, err)
	}
	waitFor(t, "request handover", func() bool {
		s, err := relay.mb.DoorStatus(ctx, "openai/dot", req.ID)
		return err == nil && s.Handover != nil && s.Handover.State == "handed" && s.Receiver != nil
	})
	item := inbox(t, grok, "tokdev")[0]
	if item.From != "openai/dot" || item.Message.Origin != "openai/dot" || item.Message.FromAgent != "dot" {
		t.Fatalf("principal = %+v", item)
	}
	select {
	case body := <-rings:
		if body.From != "openai/dot" || body.Messages[0].From != "openai/dot" {
			t.Fatalf("ring = %+v", body)
		}
	case <-time.After(time.Second):
		t.Fatal("no ring")
	}
	if _, err := relay.mb.DoorStatus(ctx, "anthropic/dot", req.ID); !errors.Is(err, mailbox.ErrNotFound) {
		t.Fatalf("leaked outbox: %v", err)
	}
	if _, err := relay.mb.FromDoor("anthropic/dot", envelope.Envelope{ID: req.ID, Kind: envelope.Notice, To: "grokbot/tokdev"}); err == nil {
		t.Fatal("another Door agent reused an outbox id")
	}
	// Only the forwarding Relay can ask for the Door agent's progress.
	progressURL := grok.srv.URL + "/v1/progress/" + req.ID + "?origin=openai%2Fdot"
	checkHTTP := func(want int) {
		t.Helper()
		res, err := http.Get(progressURL)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != want {
			b, _ := io.ReadAll(res.Body)
			t.Fatalf("progress = %d: %s", res.StatusCode, b)
		}
	}
	checkHTTP(200)
	grok.callerIs("plain", relayGrant(), tailnet.Grant{To: []string{"*"}})
	checkHTTP(404)
	grok.callerIs("imac", tailnet.Grant{To: []string{"*"}})
	checkHTTP(403)
	grok.callerIs("imac", relayGrant(), tailnet.Grant{To: []string{"*"}})

	// A Result with a file goes through the Relay, retaining the Grok Principal.
	file := addFile(t, grok, "answer.txt", "42")
	before := wake.calls.Load()
	result := grok.send(t, envelope.Envelope{Kind: envelope.Result, To: "openai/dot", FromAgent: "tokdev", ReplyTo: req.ID,
		FromThread: "grok-thread", ToThread: "dot-thread", Body: envelope.Body{State: envelope.Completed, Text: "42", Files: []envelope.File{file}}})
	waitFor(t, "Result", func() bool {
		s, err := relay.mb.DoorStatus(ctx, "openai/dot", req.ID)
		return err == nil && s.Result != nil && s.Result.ID == result.ID
	})
	received, err := relay.mb.DoorRead("openai/dot", result.ID)
	if err != nil || received.From != "grokbot/tokdev" || len(received.Files) != 1 {
		t.Fatalf("Result = %+v, %v", received, err)
	}
	data, err := os.ReadFile(received.Files[0].Path)
	if err != nil || string(data) != "42" {
		t.Fatalf("file = %q, %v", data, err)
	}
	if wake.calls.Load() <= before {
		t.Fatal("Door event not woken")
	}
	waitFor(t, "Grok status through Relay", func() bool {
		s, err := grok.mb.SentStatus(ctx, result.ID)
		return err == nil && s.Handover.State == "handed" && s.Handover.Via == "imac" && s.Receiver != nil && s.Receiver.ReadAt != nil
	})

	// The Relay's broad To grant cannot expand the Dot's door list.
	refused, err := relay.mb.FromDoor("openai/dot", envelope.Envelope{Kind: envelope.Notice, To: "grokbot/other"})
	if err != nil || !refused.Queued {
		t.Fatalf("must let receiver decide: %+v, %v", refused, err)
	}
	waitFor(t, "refusal notice to Dot", func() bool {
		for _, item := range inbox(t, relay, "openai/dot") {
			if strings.Contains(item.Message.Body.Text, refused.ID) && strings.Contains(item.Message.Body.Text, "may not message grokbot/other") {
				return true
			}
		}
		return false
	})
	if len(inbox(t, grok, "other")) != 0 {
		t.Fatal("door list bypassed")
	}
	if len(inbox(t, relay, "dot")) != 0 {
		t.Fatal("failure went to a local dot inbox")
	}

	// Another Door agent on the same Relay is addressed by its full name.
	local, err := relay.mb.FromDoor("openai/dot", envelope.Envelope{Kind: envelope.Notice, To: "openai/other"})
	if err != nil || local.Queued || len(inbox(t, relay, "openai/other")) != 1 {
		t.Fatalf("Door to Door: %+v, %v", local, err)
	}
}

func TestIncomingRelayRefusalsAndFiles(t *testing.T) {
	n := newNet(t)
	relay := n.add("imac", "100.64.0.1", nodeOpts{agents: map[string]agent.Adapter{"me": nil}, door: relayPolicy{}})
	grok := n.add("grokbot", "100.64.0.2", nodeOpts{agents: map[string]agent.Adapter{"*": nil}})
	n.add("plain", "100.64.0.3", nodeOpts{agents: map[string]agent.Adapter{"me": nil}})
	for _, tc := range []struct {
		name               string
		target             *node
		peer               string
		grants             []tailnet.Grant
		to, origin, reason string
	}{
		{"non Relay origin", grok, "plain", []tailnet.Grant{{To: []string{"*"}}}, "grokbot/tokdev", "openai/dot", "no relay: true"},
		{"tailnet origin", grok, "imac", []tailnet.Grant{{Relay: true, Door: map[string][]string{"plain/x": {"*"}}}}, "grokbot/tokdev", "plain/x", "tailnet machine"},
		{"own tailnet origin", grok, "imac", []tailnet.Grant{relayGrant()}, "grokbot/tokdev", "grokbot/x", "tailnet machine"},
		{"missing Door agent", grok, "imac", []tailnet.Grant{relayGrant()}, "grokbot/tokdev", "openai/missing", "not named"},
		{"door map without relay", grok, "imac", []tailnet.Grant{{Relay: true}, {Door: map[string][]string{"openai/dot": {"*"}}}}, "grokbot/tokdev", "openai/dot", "not named"},
		{"malformed origin", grok, "imac", []tailnet.Grant{relayGrant()}, "grokbot/tokdev", "bad", "bad origin"},
		{"Door permission", grok, "imac", []tailnet.Grant{relayGrant(), {To: []string{"*"}}}, "grokbot/other", "openai/dot", "may not message"},
		{"full address required", relay, "grokbot", []tailnet.Grant{{To: []string{"dot"}}}, "openai/dot", "", "may not message openai/dot"},
		{"unheld name", relay, "grokbot", []tailnet.Grant{{To: []string{"*"}}}, "openai/missing", "", "Relay doesn't hold"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.target.callerIs(tc.peer, tc.grants...)
			env := envelope.Envelope{V: 1, ID: "refused", Kind: envelope.Notice, To: tc.to, FromAgent: "tokdev", Origin: tc.origin}
			body, _ := json.Marshal(env)
			res, err := http.Post(tc.target.srv.URL+"/v1/push", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			var pr pushResponse
			json.NewDecoder(res.Body).Decode(&pr)
			res.Body.Close()
			if !pr.Rejected || !strings.Contains(pr.Error, tc.reason) {
				t.Fatalf("push = %+v", pr)
			}
			// The file handler must refuse before reading the bytes.
			req := httptest.NewRequest("PUT", "/v1/files/"+strings.Repeat("0", 64)+"?address="+url.QueryEscape(tc.to)+"&origin="+url.QueryEscape(tc.origin), nil)
			bodyUnread := &unreadableBody{}
			req.Body, req.ContentLength = bodyUnread, 3
			rec := httptest.NewRecorder()
			Handler(tc.target.mb, tc.target.identity).ServeHTTP(rec, req)
			var fr fileResponse
			json.Unmarshal(rec.Body.Bytes(), &fr)
			if !fr.Rejected || !strings.Contains(fr.Error, tc.reason) || bodyUnread.read {
				t.Fatalf("file = %+v; read=%v", fr, bodyUnread.read)
			}
		})
	}
}

func TestRelayCache(t *testing.T) {
	calls := 0
	relay := "imac"
	c := &Client{FindRelay: func(context.Context) (string, error) { calls++; return relay, nil }}
	for range 2 {
		if got, err := c.Relay(context.Background(), false); err != nil || got != "imac" {
			t.Fatalf("%s %v", got, err)
		}
	}
	if calls != 1 {
		t.Fatalf("calls = %d", calls)
	}
	relay = "new-relay"
	if got, err := c.Relay(context.Background(), true); err != nil || got != relay || calls != 2 {
		t.Fatalf("refresh = %s %v, %d calls", got, err, calls)
	}
	c.relayUntil = time.Now().Add(-time.Second)
	if _, err := c.Relay(context.Background(), false); err != nil || calls != 3 {
		t.Fatalf("expiry = %v, %d calls", err, calls)
	}
}

func TestRelayRouteRefreshedAfterFailure(t *testing.T) {
	n := newNet(t)
	old := n.add("imac", "100.64.0.1", nodeOpts{agents: map[string]agent.Adapter{"me": nil}, door: relayPolicy{}})
	next := n.add("replacement", "100.64.0.3", nodeOpts{agents: map[string]agent.Adapter{"me": nil}, door: relayPolicy{}})
	next.callerIs("grokbot", tailnet.Grant{To: []string{"*"}})
	old.up.Store(false)
	var found atomic.Value
	found.Store("imac")
	var discoveries atomic.Int64
	grok := n.add("grokbot", "100.64.0.2", nodeOpts{agents: map[string]agent.Adapter{"*": nil}, findRelay: func(context.Context) (string, error) {
		discoveries.Add(1)
		return found.Load().(string), nil
	}})
	r := grok.send(t, envelope.Envelope{Kind: envelope.Notice, FromAgent: "tokdev", To: "openai/dot"})
	waitFor(t, "failed push", func() bool {
		s, err := grok.mb.SentStatus(context.Background(), r.ID)
		return err == nil && s.Handover.Attempts > 0
	})
	found.Store("replacement")
	waitFor(t, "new Relay", func() bool {
		s, err := grok.mb.SentStatus(context.Background(), r.ID)
		return err == nil && s.Handover.State == "handed" && s.Handover.Via == "replacement" && s.Receiver != nil
	})
	if discoveries.Load() < 2 || len(inbox(t, next, "openai/dot")) != 1 {
		t.Fatal("did not rediscover the Relay")
	}
}

func TestDoorForwardingExpiresToOriginalSender(t *testing.T) {
	n := newNet(t)
	relay := n.add("imac", "100.64.0.1", nodeOpts{agents: map[string]agent.Adapter{"me": nil}, door: relayPolicy{}, giveUp: 100 * time.Millisecond})
	grok := n.add("grokbot", "100.64.0.2", nodeOpts{agents: map[string]agent.Adapter{"*": nil}})
	grok.up.Store(false)
	r, err := relay.mb.FromDoor("openai/dot", envelope.Envelope{Kind: envelope.Request, To: "grokbot/tokdev", FromThread: "thread"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "expiry notice", func() bool { return len(inbox(t, relay, "openai/dot")) == 1 })
	notice := inbox(t, relay, "openai/dot")[0].Message
	if notice.Kind != envelope.Notice || notice.ToThread != "thread" || !strings.Contains(notice.Body.Text, r.ID) || !strings.Contains(notice.Body.Text, "could not be confirmed") {
		t.Fatalf("notice = %+v", notice)
	}
	s, err := relay.mb.DoorStatus(context.Background(), "openai/dot", r.ID)
	if err != nil || s.Handover.State != "failed" {
		t.Fatalf("status = %+v, %v", s, err)
	}
}

func TestForwardedFileUsesOriginPermission(t *testing.T) {
	n := newNet(t)
	grok := n.add("grokbot", "100.64.0.2", nodeOpts{agents: map[string]agent.Adapter{"*": nil}})
	grok.callerIs("imac", relayGrant()) // No To permission at all.
	_, port, _ := net.SplitHostPort(grok.srv.Listener.Addr().String())
	portNum, _ := strconv.Atoi(port)
	c := &Client{Port: portNum, Lookup: func(context.Context, string) (netip.Addr, bool, error) {
		return netip.MustParseAddr("127.0.0.1"), true, nil
	}}
	content := "a file forwarded for a Door agent"
	f := envelope.File{Name: "forwarded.txt", SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(content))), Size: int64(len(content))}
	if err := c.PutFile(context.Background(), "grokbot", "grokbot/tokdev", "openai/dot", f, strings.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	env := envelope.Envelope{V: 1, ID: "forwarded-file", Kind: envelope.Notice, To: "grokbot/tokdev", FromAgent: "spoofed", Origin: "openai/dot", Body: envelope.Body{Files: []envelope.File{f}}}
	if _, err := c.Push(context.Background(), "grokbot", env); err != nil {
		t.Fatal(err)
	}
	item, err := grok.mb.Read("tokdev", env.ID)
	if err != nil || item.From != "openai/dot" || item.Message.FromAgent != "dot" || len(item.Files) != 1 {
		t.Fatalf("item = %+v, %v", item, err)
	}
	b, err := os.ReadFile(item.Files[0].Path)
	if err != nil || string(b) != content {
		t.Fatalf("file = %q, %v", b, err)
	}
}

func TestRelayDiscoveryErrorsRefuseSend(t *testing.T) {
	for _, reason := range []string{"no Relay found in the Tailscale policy", "multiple Relays found in the Tailscale policy: [one two]; baf needs exactly one Relay"} {
		t.Run(reason, func(t *testing.T) {
			n := newNet(t)
			grok := n.add("grokbot", "100.64.0.2", nodeOpts{agents: map[string]agent.Adapter{"*": nil}, findRelay: func(context.Context) (string, error) { return "", errors.New(reason) }})
			_, err := grok.mb.Send(envelope.Envelope{Kind: envelope.Notice, FromAgent: "tokdev", To: "openai/dot"})
			var rej *mailbox.Rejection
			if !errors.As(err, &rej) || !strings.Contains(err.Error(), reason) {
				t.Fatalf("send = %v", err)
			}
			out, err := grok.mb.Outbox(true, 10)
			if err != nil || len(out) != 0 {
				t.Fatalf("outbox = %+v, %v", out, err)
			}
		})
	}
}
