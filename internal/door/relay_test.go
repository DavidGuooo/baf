package door

import (
	"context"
	"net"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/DavidGuooo/baf/internal/agent"
	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/DavidGuooo/baf/internal/mailbox"
	"github.com/DavidGuooo/baf/internal/tailnet"
	"github.com/DavidGuooo/baf/internal/transport"
)

type wireIdentity struct{ peer tailnet.Peer }

func (i wireIdentity) WhoIs(context.Context, string) (tailnet.Peer, error) { return i.peer, nil }

// This exercises the actual MCP tools and signed event callback as well as
// HTTP handover between two SQLite mailboxes. No real account is contacted.
func TestMCPThroughRelayAndResultEvent(t *testing.T) {
	lookup := func(_ context.Context, node string) (netip.Addr, bool, error) {
		if node == "relay" || node == "grokbot" {
			return netip.MustParseAddr("127.0.0.1"), true, nil
		}
		return netip.Addr{}, false, tailnet.ErrNoSuchNode
	}
	outgoing := &transport.Client{Lookup: lookup}
	h := newHarnessWithTransport(t, outgoing)
	relayPeer := wireIdentity{tailnet.Peer{Node: "relay", Grants: []tailnet.Grant{
		{Relay: true, Door: map[string][]string{"openai/dot": {"tokdev"}}}, {To: []string{"*"}},
	}}}
	grokPeer := wireIdentity{tailnet.Peer{Node: "grokbot", Grants: []tailnet.Grant{{To: []string{"openai/dot"}}}}}
	relayHTTP := httptest.NewServer(transport.Handler(h.mailbox, grokPeer))
	defer relayHTTP.Close()
	port := func(s *httptest.Server) int {
		_, p, _ := net.SplitHostPort(s.Listener.Addr().String())
		n, _ := strconv.Atoi(p)
		return n
	}
	returning := &transport.Client{Lookup: lookup, Port: port(relayHTTP), FindRelay: func(context.Context) (string, error) { return "relay", nil }}
	grok, err := mailbox.Open(mailbox.Options{Path: filepath.Join(t.TempDir(), "grok.db"), Node: "grokbot", Agents: map[string]agent.Adapter{"*": nil}, Transport: returning})
	if err != nil {
		t.Fatal(err)
	}
	defer grok.Close()
	grokHTTP := httptest.NewServer(transport.Handler(grok, relayPeer))
	defer grokHTTP.Close()
	outgoing.Port = port(grokHTTP)
	tok := h.token(codeForm(h.code("openai/dot")), 200)
	cb := newEventCallback(t, h.s)
	h.subscribeCallback(cb, tok.Access)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 3)
	go func() { h.mailbox.Run(ctx); done <- struct{}{} }()
	go func() { grok.Run(ctx); done <- struct{}{} }()
	go func() { h.s.RunEvents(ctx); done <- struct{}{} }()
	defer func() {
		cancel()
		for range 3 {
			<-done
		}
	}()
	await := func(f func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !f() {
			if time.Now().After(deadline) {
				t.Fatal("timed out")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	req := decodeResult[mailbox.Receipt](t, h.call("openai/dot", tok.Access, "request", map[string]any{"to": "grokbot/tokdev", "text": "calculate", "from_thread": "dot-thread"}))
	await(func() bool {
		s := decodeResult[mailbox.SentStatus](t, h.call("openai/dot", tok.Access, "status", map[string]any{"id": req.ID}))
		return s.Handover != nil && s.Handover.State == "handed" && s.Receiver != nil
	})
	result, err := grok.Send(envelope.Envelope{Kind: envelope.Result, FromAgent: "tokdev", To: "openai/dot", ReplyTo: req.ID, ToThread: "dot-thread", Body: envelope.Body{Text: "42", State: envelope.Completed}})
	if err != nil {
		t.Fatal(err)
	}
	event := takeEvent(t, cb)
	if event.Data.ID != result.ID || event.Data.From != "grokbot/tokdev" || event.Data.Text != "42" {
		t.Fatalf("event = %+v", event)
	}
	s := decodeResult[mailbox.SentStatus](t, h.call("openai/dot", tok.Access, "status", map[string]any{"id": req.ID}))
	if s.Result == nil || s.Result.ID != result.ID {
		t.Fatalf("status = %+v", s)
	}

	// A Request the other way is answered using the MCP reply tool. Its
	// stored Principal and threads, rather than caller-supplied fields, route it.
	reverse, err := grok.Send(envelope.Envelope{Kind: envelope.Request, FromAgent: "tokdev", To: "openai/dot", FromThread: "grok-thread", Body: envelope.Body{Text: "answer me"}})
	if err != nil {
		t.Fatal(err)
	}
	event = takeEvent(t, cb)
	if event.Data.ID != reverse.ID {
		t.Fatalf("event = %+v", event)
	}
	answer := decodeResult[mailbox.Receipt](t, h.call("openai/dot", tok.Access, "reply", map[string]any{"reply_to": reverse.ID, "text": "done"}))
	await(func() bool {
		status, err := grok.SentStatus(ctx, reverse.ID)
		return err == nil && status.Result != nil && status.Result.ID == answer.ID
	})
	item, err := grok.Read("tokdev", answer.ID)
	if err != nil || item.From != "openai/dot" || item.Message.ToThread != "grok-thread" || item.Message.Body.State != envelope.Completed {
		t.Fatalf("reply = %+v, %v", item, err)
	}
}
