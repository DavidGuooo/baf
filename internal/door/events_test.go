package door

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/DavidGuooo/baf/internal/mailbox"
)

var callbackKey = []byte("a test key with at least 24 bytes")
var callbackSecret = "whsec_" + base64.StdEncoding.EncodeToString(callbackKey)

type eventCallback struct {
	server        *httptest.Server
	key           []byte
	badAnswer     atomic.Bool
	status        atomic.Int64
	verifications atomic.Int64
	events        chan event
}

func newEventCallback(t *testing.T, s *Server) *eventCallback {
	t.Helper()
	cb := &eventCallback{key: callbackKey, events: make(chan event, 50)}
	cb.status.Store(200)
	cb.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		// Check the wire format independently of sign, just as the vendor does.
		mac := hmac.New(sha256.New, cb.key)
		fmt.Fprintf(mac, "%s.%s.", r.Header.Get("webhook-id"), r.Header.Get("webhook-timestamp"))
		mac.Write(body)
		want := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
		if r.Header.Get("webhook-signature") != want {
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("X-MCP-Subscription-Id") == "" {
			t.Error("missing event headers")
		}
		var challenge struct{ Type, Challenge string }
		json.Unmarshal(body, &challenge)
		if challenge.Type == "verification" {
			cb.verifications.Add(1)
			if cb.badAnswer.Load() {
				challenge.Challenge = "wrong"
			}
			writeJSON(w, 200, map[string]string{"challenge": challenge.Challenge})
			return
		}
		var e event
		if err := json.Unmarshal(body, &e); err != nil {
			t.Error(err)
		}
		if r.Header.Get("webhook-id") != e.ID {
			t.Error("webhook id differs from event id")
		}
		cb.events <- e
		w.WriteHeader(int(cb.status.Load()))
	}))
	t.Cleanup(cb.server.Close)
	// Only replace transport to trust the test certificate and permit loopback.
	// Production's redirect refusal and timeout still apply.
	s.callbacks.Transport = cb.server.Client().Transport
	return cb
}

func eventParams(url, secret string) map[string]any {
	return map[string]any{"name": eventName, "arguments": map[string]any{},
		"delivery": delivery{Mode: "webhook", URL: url, Secret: secret}}
}

func (h *harness) eventRPC(agent, token, method string, params any) rpcResponse {
	h.t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		h.t.Fatal(err)
	}
	return h.rpc(agent, token, string(body), 200)
}

func (h *harness) subscribeCallback(cb *eventCallback, token string) string {
	h.t.Helper()
	out := h.eventRPC("openai/dot", token, "events/subscribe", eventParams(cb.server.URL, callbackSecret))
	if out.Error != nil {
		h.t.Fatalf("subscribe: %+v", out.Error)
	}
	var sub struct{ ID, RefreshBefore string }
	if err := json.Unmarshal(out.Result, &sub); err != nil {
		h.t.Fatal(err)
	}
	if sub.ID == "" || sub.RefreshBefore == "" {
		h.t.Fatalf("subscription: %s", out.Result)
	}
	return sub.ID
}

func countRows(t *testing.T, s *Server, table string, want int) {
	t.Helper()
	var got int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s rows = %d, want %d", table, got, want)
	}
}

func TestEventSubscriptionVerification(t *testing.T) {
	h := newHarness(t)
	tok := h.token(codeForm(h.code("openai/dot")), 200)
	cb := newEventCallback(t, h.s)
	cb.badAnswer.Store(true)
	out := h.eventRPC("openai/dot", tok.Access, "events/subscribe", eventParams(cb.server.URL, callbackSecret))
	if out.Error == nil || out.Error.Code != -32015 {
		t.Fatalf("wrong challenge accepted: %+v", out)
	}
	countRows(t, h.s, "subscriptions", 0)
	cb.badAnswer.Store(false)
	wrongSecret := "whsec_" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	out = h.eventRPC("openai/dot", tok.Access, "events/subscribe", eventParams(cb.server.URL, wrongSecret))
	if out.Error == nil || !strings.Contains(out.Error.Message, "HTTP 401") {
		t.Fatalf("bad signature accepted: %+v", out)
	}
	countRows(t, h.s, "subscriptions", 0)
	id := h.subscribeCallback(cb, tok.Access)
	var expires int64
	if err := h.s.db.QueryRow(`SELECT expires_at FROM subscriptions WHERE id=?`, id).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if expires != h.s.now().Add(24*time.Hour).UnixMilli() {
		t.Fatalf("default expiry = %v", time.UnixMilli(expires))
	}
	// Renewing must verify the callback again, including a changed secret.
	if again := h.subscribeCallback(cb, tok.Access); again != id {
		t.Fatal("renewal changed subscription id")
	}
	if cb.verifications.Load() != 3 {
		t.Fatalf("verification count = %d", cb.verifications.Load())
	}
	countRows(t, h.s, "subscriptions", 1)
}

func TestEventSubscriptionTTL(t *testing.T) {
	for _, tc := range []struct {
		name string
		ttl  any
		want time.Duration
	}{
		{"null", nil, maxSubscriptionTTL}, {"short", 1, time.Minute},
		{"long", int64(1 << 62), maxSubscriptionTTL}, {"explicit", 120000, 2 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tok := h.token(codeForm(h.code("openai/dot")), 200)
			cb := newEventCallback(t, h.s)
			params := eventParams(cb.server.URL, callbackSecret)
			params["ttlMs"] = tc.ttl
			out := h.eventRPC("openai/dot", tok.Access, "events/subscribe", params)
			if out.Error != nil {
				t.Fatalf("subscribe: %+v", out.Error)
			}
			var expires int64
			if err := h.s.db.QueryRow(`SELECT expires_at FROM subscriptions`).Scan(&expires); err != nil {
				t.Fatal(err)
			}
			if expires != h.s.now().Add(tc.want).UnixMilli() {
				t.Fatalf("expiry = %v", time.UnixMilli(expires))
			}
		})
	}
}

func TestEventsIgnoreLegacyConnectorExpiry(t *testing.T) {
	h := newHarness(t)
	tok := h.token(codeForm(h.code("openai/dot")), 200)
	cb := newEventCallback(t, h.s)
	// A connector made by the old door would have cut this subscription short.
	if _, err := h.s.db.Exec(`UPDATE connectors SET expires_at=?`, h.s.now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	id := h.subscribeCallback(cb, tok.Access)
	var expires int64
	if err := h.s.db.QueryRow(`SELECT expires_at FROM subscriptions WHERE id=?`, id).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if expires != h.s.now().Add(24*time.Hour).UnixMilli() {
		t.Fatalf("subscription capped at %v", time.UnixMilli(expires))
	}
	h.clock.Add(3600)
	if _, err := h.mailbox.Send(envelope.Envelope{Kind: envelope.Notice, FromAgent: "me", To: "openai/dot",
		Body: envelope.Body{Text: "after the old deadline"}}); err != nil {
		t.Fatal(err)
	}
	h.pumpEvents()
	takeEvent(t, cb)
	// Renewal works after that old deadline too.
	h.subscribeCallback(cb, tok.Access)
	h.clock.Add(86400)
	h.pumpEvents()
	countRows(t, h.s, "subscriptions", 0)
}

func TestEventCallbackRestrictions(t *testing.T) {
	h := newHarness(t)
	tok := h.token(codeForm(h.code("openai/dot")), 200)
	for _, url := range []string{"http://example.com", "ftp://example.com", "https://user:pass@example.com", "https://example.com/#fragment"} {
		out := h.eventRPC("openai/dot", tok.Access, "events/subscribe", eventParams(url, callbackSecret))
		if out.Error == nil || out.Error.Code != -32602 {
			t.Fatalf("accepted %s: %+v", url, out)
		}
	}
	var reached atomic.Bool
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Store(true) }))
	defer target.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	h.s.callbacks.Transport = redirect.Client().Transport
	out := h.eventRPC("openai/dot", tok.Access, "events/subscribe", eventParams(redirect.URL, callbackSecret))
	if out.Error == nil || reached.Load() {
		t.Fatalf("followed redirect: %+v", out)
	}
	// The production transport must reject even a valid TLS callback on loopback.
	h.s.callbacks = callbackClient()
	out = h.eventRPC("openai/dot", tok.Access, "events/subscribe", eventParams(target.URL, callbackSecret))
	if out.Error == nil || reached.Load() {
		t.Fatalf("reached a private callback: %+v", out)
	}
	countRows(t, h.s, "subscriptions", 0)
}

func (h *harness) pumpEvents() {
	h.t.Helper()
	if err := h.s.queueEvents(); err != nil {
		h.t.Fatal(err)
	}
	if err := h.s.deliverEvents(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}

func takeEvent(t *testing.T, cb *eventCallback) event {
	t.Helper()
	select {
	case e := <-cb.events:
		return e
	case <-time.After(3 * time.Second):
		t.Fatal("no event")
		return event{}
	}
}

func noEvent(t *testing.T, cb *eventCallback) {
	t.Helper()
	select {
	case e := <-cb.events:
		t.Fatalf("unexpected event: %+v", e)
	default:
	}
}

func TestEventWholeMessageAndNoRead(t *testing.T) {
	h := newHarness(t)
	tok := h.token(codeForm(h.code("openai/dot")), 200)
	cb := newEventCallback(t, h.s)
	h.subscribeCallback(cb, tok.Access)
	request := decodeResult[mailbox.Receipt](t, h.call("openai/dot", tok.Access, "request",
		map[string]any{"to": "codex", "text": "question", "from_thread": "dot-thread"}))
	text := strings.Repeat("a whole answer, not a preview. ", 100)
	env := envelope.Envelope{ID: envelope.NewID(), Kind: envelope.Result, FromAgent: "codex", To: "openai/dot",
		FromThread: "codex-thread", ToThread: "dot-thread", ReplyTo: request.ID,
		Body: envelope.Body{Text: text, State: envelope.Completed}}
	if _, err := h.mailbox.Send(env); err != nil {
		t.Fatal(err)
	}
	h.pumpEvents()
	e := takeEvent(t, cb)
	if e.Name != eventName || e.Timestamp.IsZero() || e.Data.ID != env.ID || e.Data.From != "relay/codex" ||
		e.Data.To != "openai/dot" || e.Data.Kind != envelope.Result || e.Data.Text != text ||
		e.Data.FromThread != "codex-thread" || e.Data.ToThread != "dot-thread" ||
		e.Data.ReplyTo != request.ID || e.Data.State != envelope.Completed {
		t.Fatalf("incomplete event: %+v", e)
	}
	items, err := h.mailbox.DoorInbox("openai/dot", false)
	if err != nil || len(items) != 1 || items[0].Read || items[0].State != "inbox" {
		t.Fatalf("mail changed: %+v, %v", items, err)
	}
	status, err := h.mailbox.SentStatus(context.Background(), env.ID)
	if err != nil || status.Receiver == nil || status.Receiver.ReadAt != nil {
		t.Fatalf("status changed: %+v, %v", status, err)
	}
	// Sender retry is the same stored message; local mail never rings a Door agent.
	if receipt, err := h.mailbox.Send(env); err != nil || !receipt.Duplicate {
		t.Fatalf("duplicate: %+v, %v", receipt, err)
	}
	if _, err := h.mailbox.Send(envelope.Envelope{Kind: envelope.Notice, FromAgent: "me", To: "relay/codex"}); err != nil {
		t.Fatal(err)
	}
	h.pumpEvents()
	noEvent(t, cb)
	countRows(t, h.s, "event_deliveries", 0)
}

func TestEventRetryRestartAndGiveUp(t *testing.T) {
	h := newHarness(t)
	tok := h.token(codeForm(h.code("openai/dot")), 200)
	cb := newEventCallback(t, h.s)
	h.subscribeCallback(cb, tok.Access)
	cb.status.Store(503)
	if _, err := h.mailbox.Send(envelope.Envelope{Kind: envelope.Request, FromAgent: "me", To: "openai/dot", Body: envelope.Body{Text: "hello"}}); err != nil {
		t.Fatal(err)
	}
	h.pumpEvents()
	first := takeEvent(t, cb)
	countRows(t, h.s, "event_deliveries", 1)
	// Close and reopen the actual store, keeping only the fake clock and test TLS transport.
	s := h.s
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(Options{Path: h.path, PublicURL: s.publicURL, SigninURL: s.signinURL,
		Policy: s.policy, Identity: h.identity, Mailbox: h.mailbox})
	if err != nil {
		t.Fatal(err)
	}
	h.s = reopened
	h.s.now = s.now
	h.s.callbacks.Transport = cb.server.Client().Transport
	countRows(t, h.s, "subscriptions", 1)
	countRows(t, h.s, "event_deliveries", 1)
	h.pumpEvents() // not due yet
	noEvent(t, cb)
	h.clock.Add(2)
	h.pumpEvents()
	second := takeEvent(t, cb)
	if second.ID != first.ID || !reflect.DeepEqual(second.Data, first.Data) {
		t.Fatalf("retry changed event id or payload")
	}
	var attempts int
	var due int64
	if err := h.s.db.QueryRow(`SELECT attempts,next_try_at FROM event_deliveries`).Scan(&attempts, &due); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || due != h.s.now().Add(4*time.Second).UnixMilli() {
		t.Fatalf("backoff: %d, %d", attempts, due)
	}
	h.clock.Add(3601)
	h.pumpEvents()
	noEvent(t, cb)
	countRows(t, h.s, "event_deliveries", 0)
	items, err := h.mailbox.DoorInbox("openai/dot", false)
	if err != nil || len(items) != 1 {
		t.Fatalf("mail lost: %+v %v", items, err)
	}
}

func TestEventRevokeAndNewSignin(t *testing.T) {
	for _, action := range []string{"revoke", "signin", "unsubscribe"} {
		t.Run(action, func(t *testing.T) {
			h := newHarness(t)
			tok := h.token(codeForm(h.code("openai/dot")), 200)
			cb := newEventCallback(t, h.s)
			h.subscribeCallback(cb, tok.Access)
			if _, err := h.mailbox.Send(envelope.Envelope{Kind: envelope.Notice, FromAgent: "me", To: "openai/dot"}); err != nil {
				t.Fatal(err)
			}
			if err := h.s.queueEvents(); err != nil {
				t.Fatal(err)
			}
			countRows(t, h.s, "event_deliveries", 1)
			switch action {
			case "revoke":
				if err := h.s.Revoke("openai/dot"); err != nil {
					t.Fatal(err)
				}
			case "signin":
				h.code("openai/dot")
			case "unsubscribe":
				for range 2 {
					out := h.eventRPC("openai/dot", tok.Access, "events/unsubscribe", eventParams(cb.server.URL, ""))
					if out.Error != nil {
						t.Fatalf("unsubscribe: %+v", out.Error)
					}
				}
			}
			countRows(t, h.s, "subscriptions", 0)
			countRows(t, h.s, "event_deliveries", 0)
			h.pumpEvents()
			noEvent(t, cb)
		})
	}
}

func TestEventNewSubscriptionDoesNotReplayInbox(t *testing.T) {
	h := newHarness(t)
	tok := h.token(codeForm(h.code("openai/dot")), 200)
	cb := newEventCallback(t, h.s)
	if _, err := h.mailbox.Send(envelope.Envelope{Kind: envelope.Notice, FromAgent: "me", To: "openai/dot"}); err != nil {
		t.Fatal(err)
	}
	h.subscribeCallback(cb, tok.Access)
	h.pumpEvents()
	noEvent(t, cb)
}

func TestEventWakeLoopAndShutdown(t *testing.T) {
	h := newHarness(t)
	tok := h.token(codeForm(h.code("openai/dot")), 200)
	cb := newEventCallback(t, h.s)
	h.subscribeCallback(cb, tok.Access)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.s.RunEvents(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	if _, err := h.mailbox.Send(envelope.Envelope{Kind: envelope.Notice, FromAgent: "me", To: "openai/dot"}); err != nil {
		t.Fatal(err)
	}
	takeEvent(t, cb)
}

func TestEventRevokedDuringVerification(t *testing.T) {
	h := newHarness(t)
	tok := h.token(codeForm(h.code("openai/dot")), 200)
	cb := newEventCallback(t, h.s)
	base := h.s.callbacks.Transport
	h.s.callbacks.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
		resp, err := base.RoundTrip(r)
		if revokeErr := h.s.Revoke("openai/dot"); revokeErr != nil {
			t.Error(revokeErr)
		}
		return resp, err
	})
	out := h.eventRPC("openai/dot", tok.Access, "events/subscribe", eventParams(cb.server.URL, callbackSecret))
	if out.Error == nil || !strings.Contains(out.Error.Message, "connector changed") {
		t.Fatalf("revoked challenge activated: %+v", out)
	}
	countRows(t, h.s, "subscriptions", 0)
}

func TestEventAgentIsolationAndPolicy(t *testing.T) {
	h := newHarness(t)
	tok := h.token(codeForm(h.code("openai/dot")), 200)
	other := h.token(codeForm(h.code("openai/other")), 200)
	cb := newEventCallback(t, h.s)
	h.subscribeCallback(cb, tok.Access)
	params := eventParams(cb.server.URL, callbackSecret)
	out := h.eventRPC("openai/other", other.Access, "events/subscribe", params)
	if out.Error != nil {
		t.Fatalf("other subscription: %+v", out.Error)
	}
	countRows(t, h.s, "subscriptions", 2)
	out = h.eventRPC("openai/other", other.Access, "events/unsubscribe", params)
	if out.Error != nil {
		t.Fatalf("other unsubscribe: %+v", out.Error)
	}
	countRows(t, h.s, "subscriptions", 1)
	if _, err := h.mailbox.Send(envelope.Envelope{Kind: envelope.Notice, FromAgent: "me", To: "openai/other"}); err != nil {
		t.Fatal(err)
	}
	h.pumpEvents()
	noEvent(t, cb)
	if _, err := h.mailbox.Send(envelope.Envelope{Kind: envelope.Notice, FromAgent: "me", To: "openai/dot"}); err != nil {
		t.Fatal(err)
	}
	h.identity.mu.Lock()
	h.identity.removed = true
	h.identity.mu.Unlock()
	h.pumpEvents()
	noEvent(t, cb)
	countRows(t, h.s, "event_deliveries", 1)
	// A fresh policy check is made for the retry as well.
	h.identity.mu.Lock()
	h.identity.removed = false
	h.identity.mu.Unlock()
	h.clock.Add(2)
	h.pumpEvents()
	takeEvent(t, cb)
	countRows(t, h.s, "event_deliveries", 0)
}

func TestEventExpiryAndTerminalResponses(t *testing.T) {
	for _, status := range []int{410, 413, 0} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			h := newHarness(t)
			tok := h.token(codeForm(h.code("openai/dot")), 200)
			cb := newEventCallback(t, h.s)
			h.subscribeCallback(cb, tok.Access)
			if _, err := h.mailbox.Send(envelope.Envelope{Kind: envelope.Notice, FromAgent: "me", To: "openai/dot"}); err != nil {
				t.Fatal(err)
			}
			if err := h.s.queueEvents(); err != nil {
				t.Fatal(err)
			}
			countRows(t, h.s, "event_deliveries", 1)
			wantSubs := 0
			if status == 0 {
				h.clock.Add(24 * 3600)
			} else {
				cb.status.Store(int64(status))
			}
			h.pumpEvents()
			if status == 0 {
				noEvent(t, cb)
			} else {
				takeEvent(t, cb)
			}
			if status == 413 {
				wantSubs = 1
			}
			countRows(t, h.s, "subscriptions", wantSubs)
			countRows(t, h.s, "event_deliveries", 0)
		})
	}
}

func TestEventRestartBeforeQueue(t *testing.T) {
	h := newHarness(t)
	tok := h.token(codeForm(h.code("openai/dot")), 200)
	cb := newEventCallback(t, h.s)
	h.subscribeCallback(cb, tok.Access)
	r, err := h.mailbox.Send(envelope.Envelope{Kind: envelope.Request, FromAgent: "me", To: "openai/dot", Body: envelope.Body{Text: "survive the missed wake"}})
	if err != nil {
		t.Fatal(err)
	}
	other, err := Open(Options{Path: h.path, PublicURL: h.s.publicURL, SigninURL: h.s.signinURL,
		Policy: h.s.policy, Identity: h.identity, Mailbox: h.mailbox})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.callbacks.Transport = cb.server.Client().Transport
	if err := other.queueEvents(); err != nil {
		t.Fatal(err)
	}
	if err := other.deliverEvents(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e := takeEvent(t, cb); e.Data.ID != r.ID {
		t.Fatalf("recovered wrong message: %+v", e)
	}
	// The saved cursor prevents replay even after successful delivery removed the job.
	if err := other.queueEvents(); err != nil {
		t.Fatal(err)
	}
	countRows(t, other, "event_deliveries", 0)
}

func TestEventExpiredSubscriptionStartsFresh(t *testing.T) {
	h := newHarness(t)
	tok := h.token(codeForm(h.code("openai/dot")), 200)
	cb := newEventCallback(t, h.s)
	params := eventParams(cb.server.URL, callbackSecret)
	params["ttlMs"] = 60000
	if out := h.eventRPC("openai/dot", tok.Access, "events/subscribe", params); out.Error != nil {
		t.Fatal(out.Error)
	}
	if _, err := h.mailbox.Send(envelope.Envelope{Kind: envelope.Notice, FromAgent: "me", To: "openai/dot"}); err != nil {
		t.Fatal(err)
	}
	if err := h.s.queueEvents(); err != nil {
		t.Fatal(err)
	}
	countRows(t, h.s, "event_deliveries", 1)
	h.clock.Add(61)
	h.subscribeCallback(cb, tok.Access)
	countRows(t, h.s, "event_deliveries", 0)
	h.pumpEvents()
	noEvent(t, cb)
}

func TestEventLateGoneDoesNotRemoveReplacement(t *testing.T) {
	h := newHarness(t)
	tok := h.token(codeForm(h.code("openai/dot")), 200)
	cb := newEventCallback(t, h.s)
	h.subscribeCallback(cb, tok.Access)
	cb.status.Store(410)
	base := h.s.callbacks.Transport
	h.s.callbacks.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
		resp, err := base.RoundTrip(r)
		if strings.HasPrefix(r.Header.Get("webhook-id"), "evt_") {
			// Replace it while the old request is in flight, before its 410 is handled.
			params, _ := json.Marshal(eventParams(cb.server.URL, callbackSecret))
			if _, err := h.s.unsubscribe("openai/dot", params); err != nil {
				t.Error(err)
			}
			if _, err := h.s.subscribe(context.Background(), "openai/dot", params); err != nil {
				t.Error(err)
			}
		}
		return resp, err
	})
	if _, err := h.mailbox.Send(envelope.Envelope{Kind: envelope.Notice, FromAgent: "me", To: "openai/dot"}); err != nil {
		t.Fatal(err)
	}
	h.pumpEvents()
	takeEvent(t, cb)
	countRows(t, h.s, "subscriptions", 1)
	countRows(t, h.s, "event_deliveries", 0)
}
