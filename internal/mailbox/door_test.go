package mailbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DavidGuooo/baf/internal/agent"
	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/DavidGuooo/baf/internal/tailnet"
)

type fakeDoorIdentity struct {
	peer tailnet.Peer
	err  error
}

func (f *fakeDoorIdentity) SelfPeer(context.Context) (tailnet.Peer, error) {
	return f.peer, f.err
}

type doorTransport struct {
	Transport
	lookups int
}

func (f *doorTransport) Known(_ context.Context, node string) error {
	f.lookups++
	if node == "other-node" {
		return nil
	}
	return tailnet.ErrNoSuchNode
}

func doorMailbox(t *testing.T, enabled bool) (*Mailbox, *fakeDoorIdentity, *doorTransport) {
	t.Helper()
	id := &fakeDoorIdentity{peer: tailnet.Peer{Node: "box", Grants: []tailnet.Grant{{
		Relay: true, To: []string{"*"}, Door: map[string][]string{"openai/dot": {"codex", "me"}},
	}}}}
	tr := &doorTransport{}
	opts := Options{
		Path: filepath.Join(t.TempDir(), "mailbox.db"), Node: "box",
		Agents:    map[string]agent.Adapter{"codex": &fakeAgent{}, "claude": nil, "me": nil, "dot": nil, AnyAgent: nil},
		Transport: tr,
	}
	if enabled {
		opts.Door = id
	}
	mb, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mb.Close() })
	return mb, id, tr
}

func wantDoorRejection(t *testing.T, err error, text string) {
	t.Helper()
	var rej *Rejection
	if !errors.As(err, &rej) || !strings.Contains(err.Error(), text) {
		t.Fatalf("err = %v, want rejection containing %q", err, text)
	}
}

func TestFromDoorPermissionAndPrincipal(t *testing.T) {
	mb, _, tr := doorMailbox(t, true)
	env := envelope.Envelope{Kind: envelope.Notice, To: "box/codex", FromAgent: "claude", Origin: "other-node/me"}
	r, err := mb.FromDoor("openai/dot", env)
	if err != nil || r.Queued {
		t.Fatalf("receipt = %+v, err = %v", r, err)
	}
	items, err := mb.Inbox("codex", true)
	if err != nil || len(items) != 1 || items[0].From != "openai/dot" || items[0].Message.FromAgent != "dot" || items[0].Message.Origin != "" {
		t.Fatalf("inbox = %+v, err = %v", items, err)
	}
	for _, tc := range []struct{ to, reason string }{
		{"box/claude", "door entry in the Tailscale policy"},

		{"openai/dot", "door entry in the Tailscale policy"},
	} {
		env.To = tc.to
		_, err := mb.FromDoor("openai/dot", env)
		wantDoorRejection(t, err, tc.reason)
	}
	if tr.lookups != 0 {
		t.Fatal("a Door send tried to route to another machine")
	}
	out, err := mb.Outbox(true, 10)
	if err != nil || len(out) != 0 {
		t.Fatalf("outbox = %+v, err = %v", out, err)
	}
}

func TestLocalMailToDoorReadAndFiles(t *testing.T) {
	mb, _, tr := doorMailbox(t, true)
	f, err := mb.AddFile("hello.txt", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := mb.Send(envelope.Envelope{Kind: envelope.Notice, FromAgent: "codex", To: "openai/dot", Body: envelope.Body{Files: []envelope.File{f}}})
	if err != nil || r.Queued || tr.lookups != 0 {
		t.Fatalf("receipt = %+v, lookups = %d, err = %v", r, tr.lookups, err)
	}
	items, err := mb.DoorInbox("openai/dot", false)
	if err != nil || len(items) != 1 || items[0].State != "inbox" || items[0].Read || items[0].Message.To != "openai/dot" {
		t.Fatalf("inbox = %+v, err = %v", items, err)
	}
	p, err := mb.SentStatus(context.Background(), r.ID)
	if err != nil || p.Receiver.To != "openai/dot" || p.Receiver.ReadAt != nil || p.Handover != nil {
		t.Fatalf("status = %+v, err = %v", p, err)
	}
	item, err := mb.DoorRead("openai/dot", r.ID)
	if err != nil || len(item.Files) != 1 {
		t.Fatalf("read = %+v, err = %v", item, err)
	}
	b, err := os.ReadFile(item.Files[0].Path)
	if err != nil || string(b) != "hello" {
		t.Fatalf("file = %q, err = %v", b, err)
	}
	p, err = mb.SentStatus(context.Background(), r.ID)
	if err != nil || p.Receiver.ReadAt == nil || p.Receiver.Run != nil || p.Receiver.State != "inbox" {
		t.Fatalf("opened status = %+v, err = %v", p, err)
	}
	if unread, err := mb.DoorInbox("openai/dot", false); err != nil || len(unread) != 0 {
		t.Fatalf("unread = %+v, err = %v", unread, err)
	}
	if all, err := mb.DoorInbox("openai/dot", true); err != nil || len(all) != 1 {
		t.Fatalf("reading deleted mail: %+v, %v", all, err)
	}
	if local, err := mb.Inbox("dot", true); err != nil || len(local) != 0 {
		t.Fatalf("Door mail went to the local dot: %+v, %v", local, err)
	}
	claims, err := mb.claim()
	if err != nil || len(claims) != 0 {
		t.Fatalf("Door agent got a run: %+v, %v", claims, err)
	}
	_, err = mb.FromDoor("openai/dot", envelope.Envelope{Kind: envelope.Notice, To: "box/me", Body: envelope.Body{Files: []envelope.File{f}}})
	wantDoorRejection(t, err, "files from Door agents are not supported yet")
}

func TestDoorRequestResultAndThreads(t *testing.T) {
	mb, _, _ := doorMailbox(t, true)
	env := envelope.Envelope{ID: "request", Kind: envelope.Request, To: "box/codex", FromThread: "dot-thread"}
	r, err := mb.FromDoor("openai/dot", env)
	if err != nil {
		t.Fatal(err)
	}
	again, err := mb.FromDoor("openai/dot", env)
	if err != nil || !again.Duplicate || again.StoredAt.UnixMilli() != r.StoredAt.UnixMilli() {
		t.Fatalf("duplicate = %+v, err = %v", again, err)
	}
	claims, err := mb.claim()
	if err != nil || len(claims) != 1 || claims[0].sender.String() != "openai/dot" {
		t.Fatalf("claims = %+v, err = %v", claims, err)
	}
	mb.execute(context.Background(), claims[0])
	items, err := mb.DoorInbox("openai/dot", true)
	if err != nil || len(items) != 1 {
		t.Fatalf("inbox = %+v, err = %v", items, err)
	}
	res := items[0].Message
	if res.Kind != envelope.Result || res.To != "openai/dot" || res.ReplyTo != r.ID || res.ToThread != "dot-thread" || res.FromThread != "t1" || res.Body.State != envelope.Completed {
		t.Fatalf("result = %+v", res)
	}
	s, err := mb.DoorStatus(context.Background(), "openai/dot", r.ID)
	if err != nil || s.Result == nil || s.Result.ID != res.ID || s.Receiver.Run.State != "completed" || s.Handover != nil {
		t.Fatalf("status = %+v, err = %v", s, err)
	}
	_, err = mb.FromDoor("openai/dot", envelope.Envelope{Kind: envelope.Notice, To: "box/codex", ToThread: res.FromThread})
	if err != nil {
		t.Fatalf("continue shared Codex thread: %v", err)
	}
	_, err = mb.FromDoor("openai/dot", envelope.Envelope{Kind: envelope.Notice, To: "box/codex", ToThread: "unshared"})
	wantDoorRejection(t, err, "never shared thread")
	_, err = mb.Send(envelope.Envelope{Kind: envelope.Notice, FromAgent: "me", To: "openai/dot", ToThread: "dot-thread"})
	wantDoorRejection(t, err, "never shared thread")
	out, err := mb.Outbox(true, 10)
	if err != nil || len(out) != 0 {
		t.Fatalf("outbox = %+v, err = %v", out, err)
	}
}

func TestDoorPolicyChangesAndFailure(t *testing.T) {
	mb, id, _ := doorMailbox(t, true)
	env := envelope.Envelope{Kind: envelope.Notice, To: "box/me"}
	if _, err := mb.FromDoor("openai/dot", env); err != nil {
		t.Fatal(err)
	}
	id.peer.Grants[0].Door["openai/dot"] = []string{"codex"}
	_, err := mb.FromDoor("openai/dot", env)
	wantDoorRejection(t, err, "door entry")
	delete(id.peer.Grants[0].Door, "openai/dot")
	for name, call := range map[string]func() error{
		"send": func() error { _, err := mb.FromDoor("openai/dot", env); return err },
		"receive": func() error {
			_, err := mb.Send(envelope.Envelope{Kind: envelope.Notice, FromAgent: "codex", To: "openai/dot"})
			return err
		},
		"inbox":  func() error { _, err := mb.DoorInbox("openai/dot", true); return err },
		"read":   func() error { _, err := mb.DoorRead("openai/dot", "id"); return err },
		"status": func() error { _, err := mb.DoorStatus(context.Background(), "openai/dot", "id"); return err },
	} {
		t.Run(name, func(t *testing.T) { wantDoorRejection(t, call(), "policy does not list") })
	}
	id.peer.Grants[0].Door["openai/dot"] = []string{"me"}
	id.err = errors.New("Tailscale unavailable")
	if _, err := mb.FromDoor("openai/dot", env); !errors.Is(err, id.err) {
		t.Fatalf("used stale permission after WhoIs failed: %v", err)
	}
	if _, err := mb.Send(envelope.Envelope{Kind: envelope.Notice, FromAgent: "codex", To: "openai/dot"}); !errors.Is(err, id.err) {
		t.Fatalf("used stale destination after WhoIs failed: %v", err)
	}
	id.err = nil
	r, err := mb.Send(envelope.Envelope{Kind: envelope.Notice, FromAgent: "codex", To: "other-node/me"})
	if err != nil || !r.Queued {
		t.Fatalf("ordinary tailnet send = %+v, err = %v", r, err)
	}
}

func TestDoorDisabled(t *testing.T) {
	mb, _, _ := doorMailbox(t, false)
	_, err := mb.FromDoor("openai/dot", envelope.Envelope{Kind: envelope.Notice, To: "box/codex"})
	wantDoorRejection(t, err, "disabled")
	_, err = mb.Send(envelope.Envelope{Kind: envelope.Notice, FromAgent: "codex", To: "openai/dot"})
	if err == nil {
		t.Fatal("disabled door accepted local mail for a Door agent")
	}
	_, err = mb.DoorInbox("openai/dot", true)
	wantDoorRejection(t, err, "disabled")
	_, err = mb.DoorRead("openai/dot", "id")
	wantDoorRejection(t, err, "disabled")
	_, err = mb.DoorStatus(context.Background(), "openai/dot", "id")
	wantDoorRejection(t, err, "disabled")
	_, err = mb.DoorAgents()
	wantDoorRejection(t, err, "disabled")
	// The wildcard for local inboxes must not let the owner impersonate a Dot.
	_, err = mb.Send(envelope.Envelope{Kind: envelope.Notice, FromAgent: "openai/dot", To: "box/me"})
	wantDoorRejection(t, err, "not an agent")
	items, err := mb.Inbox("", true)
	if err != nil || len(items) != 0 {
		t.Fatalf("inbox = %+v, err = %v", items, err)
	}
}

func TestDoorMailIsScopedToItsPrincipal(t *testing.T) {
	mb, id, _ := doorMailbox(t, true)
	id.peer.Grants[0].Door["openai/other"] = []string{"me"}
	env := envelope.Envelope{ID: "same-id", Kind: envelope.Notice, To: "box/me"}
	if _, err := mb.FromDoor("openai/dot", env); err != nil {
		t.Fatal(err)
	}
	if _, err := mb.DoorStatus(context.Background(), "openai/other", env.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other Door agent saw status: %v", err)
	}
	_, err := mb.FromDoor("openai/other", env)
	wantDoorRejection(t, err, "already used by another agent")
	// The local message with this id is a different message; DoorRead must
	// find its own inbox entry rather than the earlier message for me.
	if _, err := mb.Send(envelope.Envelope{ID: env.ID, Kind: envelope.Notice, FromAgent: "me", To: "openai/dot"}); err != nil {
		t.Fatal(err)
	}
	item, err := mb.DoorRead("openai/dot", env.ID)
	if err != nil || item.From != "box/me" || item.Message.To != "openai/dot" {
		t.Fatalf("read = %+v, err = %v", item, err)
	}
	if _, err := mb.DoorRead("openai/other", env.ID); !errors.Is(err, ErrNotForYou) {
		t.Fatalf("other Door agent read mail: %v", err)
	}
	if _, err := mb.Accept(envelope.Envelope{V: 1, ID: "tailnet", Kind: envelope.Notice, FromAgent: "me", To: "openai/dot"}, Principal{Node: "other-node", Agent: "me"}); err != nil {
		t.Fatal(err)
	}
}

func TestDoorStatusDoesNotUseAnotherSendWithTheSameID(t *testing.T) {
	mb, id, _ := doorMailbox(t, true)
	id.peer.Grants[0].Door["anthropic/dot"] = []string{"codex"}
	// The outbox and two vendors can all have the same sender-chosen id.
	if _, err := mb.Send(envelope.Envelope{ID: "shared-id", Kind: envelope.Notice, FromAgent: "me", To: "other-node/me"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"openai/dot", "anthropic/dot"} {
		if _, err := mb.FromDoor(name, envelope.Envelope{ID: "shared-id", Kind: envelope.Request, To: "box/codex"}); err != nil {
			t.Fatal(err)
		}
	}
	claims, err := mb.claim()
	if err != nil || len(claims) != 2 {
		t.Fatalf("claims = %+v, err = %v", claims, err)
	}
	mb.execute(context.Background(), claims[0])
	other, err := mb.DoorStatus(context.Background(), "anthropic/dot", "shared-id")
	if err != nil || other.Result != nil || other.Handover != nil || other.To != "box/codex" {
		t.Fatalf("status picked another sender's outbox or Result: %+v, %v", other, err)
	}
	mb.execute(context.Background(), claims[1])
	for _, name := range []string{"openai/dot", "anthropic/dot"} {
		items, err := mb.DoorInbox(name, true)
		if err != nil || len(items) != 1 {
			t.Fatalf("%s inbox = %+v, err = %v", name, items, err)
		}
		s, err := mb.DoorStatus(context.Background(), name, "shared-id")
		if err != nil || s.Result == nil || s.Result.ID != items[0].Message.ID || s.Handover != nil {
			t.Fatalf("%s status = %+v, err = %v", name, s, err)
		}
	}
}

func TestDoorRequestRecoveredAfterCrash(t *testing.T) {
	mb, id, tr := doorMailbox(t, true)
	r, err := mb.FromDoor("openai/dot", envelope.Envelope{Kind: envelope.Request, To: "box/codex", FromThread: "dot-thread"})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := mb.claim()
	if err != nil || len(claims) != 1 {
		t.Fatalf("claims = %+v, err = %v", claims, err)
	}
	path := filepath.Join(filepath.Dir(mb.filesDir), "mailbox.db")
	if err := mb.Close(); err != nil {
		t.Fatal(err)
	}
	mb, err = Open(Options{Path: path, Node: "box", Agents: mb.agents, Door: id, Transport: tr})
	if err != nil {
		t.Fatal(err)
	}
	defer mb.Close()
	items, err := mb.DoorInbox("openai/dot", true)
	if err != nil || len(items) != 1 || items[0].Message.ReplyTo != r.ID || items[0].Message.Body.State != envelope.Unknown || items[0].Message.ToThread != "dot-thread" {
		t.Fatalf("recovered Result = %+v, err = %v", items, err)
	}
	if claims, err := mb.claim(); err != nil || len(claims) != 0 {
		t.Fatalf("recovery repeated a run: %+v, %v", claims, err)
	}
}

func TestForwardingNodeSurvivesRestart(t *testing.T) {
	mb, _, tr := doorMailbox(t, true)
	path := filepath.Join(filepath.Dir(mb.filesDir), "mailbox.db")
	env := envelope.Envelope{V: 1, ID: "forwarded", Kind: envelope.Notice, FromAgent: "dot", Origin: "openai/dot", To: "box/me"}
	from := Principal{Node: "openai", Agent: "dot"}
	if _, err := mb.AcceptForwarded(env, from, "relay"); err != nil {
		t.Fatal(err)
	}
	// A duplicate cannot transfer progress ownership to a different Relay.
	if _, err := mb.AcceptForwarded(env, from, "another"); err != nil {
		t.Fatal(err)
	}
	if err := mb.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(Options{Path: path, Node: "box", Agents: map[string]agent.Adapter{"me": nil}, Transport: tr})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if p, err := reopened.ForwardedProgress("relay", "openai/dot", env.ID); err != nil || p.Origin != "openai/dot" {
		t.Fatalf("progress = %+v, %v", p, err)
	}
	for _, relay := range []string{"another", "plain"} {
		if _, err := reopened.ForwardedProgress(relay, "openai/dot", env.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("progress leaked to %s: %v", relay, err)
		}
	}
}

func TestOutboxAs(t *testing.T) {
	mb, policy, _ := doorMailbox(t, true)
	policy.peer.Grants[0].Door["anthropic/dot"] = []string{"me"}
	policy.peer.Grants[0].Door["openai/other"] = []string{"me"}
	policy.peer.Grants[0].Door["openai/dot"] = []string{"me", "openai/other"}
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	mb.now = func() time.Time { return now }
	send := func(id, as, to string) Receipt {
		t.Helper()
		now = now.Add(time.Second)
		env := envelope.Envelope{ID: id, Kind: envelope.Notice, FromAgent: as, To: to}
		var r Receipt
		var err error
		if strings.Contains(as, "/") {
			r, err = mb.FromDoor(as, env)
		} else {
			r, err = mb.Send(env)
		}
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	send("plain-local", "me", "box/codex")
	send("plain-remote", "me", "other-node/me")
	send("same-name", "dot", "other-node/me")
	send("other-vendor", "anthropic/dot", "box/me")
	send("other-door", "openai/other", "box/me")
	send("other-forwarded", "anthropic/dot", "other-node/me")
	send("handed", "openai/dot", "other-node/me")
	if _, err := mb.db.Exec(`UPDATE outbox SET state='handed', done_at=created_at WHERE id='handed'`); err != nil {
		t.Fatal(err)
	}
	local := send("local", "openai/dot", "box/me")
	send("waiting", "openai/dot", "other-node/me")
	send("same-vendor", "openai/dot", "openai/other")
	for _, tc := range []struct {
		as    string
		all   bool
		limit int
		ids   []string
	}{
		{"me", true, 50, []string{"plain-remote"}},
		{"dot", true, 50, []string{"same-name"}},
		{"openai/dot", true, 50, []string{"waiting", "local", "handed"}},
		{"openai/dot", true, 2, []string{"waiting", "local"}},
		{"openai/dot", false, 50, []string{"waiting"}},
		{"", true, 50, []string{"waiting", "handed", "other-forwarded", "same-name", "plain-remote"}},
	} {
		items, err := mb.OutboxAs(tc.as, tc.all, tc.limit)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, it := range items {
			ids = append(ids, it.ID)
			if it.ID == local.ID && (it.State != "handed" || it.Via != "box" || it.Origin != "openai/dot" ||
				!it.CreatedAt.Equal(local.StoredAt) || it.DoneAt == nil || !it.DoneAt.Equal(local.StoredAt)) {
				t.Fatalf("local handover = %+v", it)
			}
		}
		if !slices.Equal(ids, tc.ids) {
			t.Fatalf("outbox %q all=%v limit=%d: %v, want %v", tc.as, tc.all, tc.limit, ids, tc.ids)
		}
	}
	_, err := mb.OutboxAs("openai/missing", false, 50)
	wantDoorRejection(t, err, "does not list")
	plain, _, _ := doorMailbox(t, false)
	if items, err := plain.OutboxAs("openai/dot", true, 50); err != nil || len(items) != 0 {
		t.Fatalf("non-Relay outbox = %+v, %v", items, err)
	}
}

func TestOutboxAsOnNodeWithoutRelayGrant(t *testing.T) {
	mb, policy, _ := doorMailbox(t, true)
	policy.peer.Grants[0].Relay = false
	items, err := mb.OutboxAs("openai/missing", true, 50)
	if err != nil || len(items) != 0 {
		t.Fatalf("outbox on ordinary node: %+v, %v", items, err)
	}
}
