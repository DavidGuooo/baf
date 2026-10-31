package transport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DavidGuooo/baf/internal/agent"
	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/DavidGuooo/baf/internal/mailbox"
	"github.com/DavidGuooo/baf/internal/tailnet"
)

// These tests run two real mailboxes, "imac" and "grok", each behind its own
// HTTP listener, like two machines on a tailnet. Tailscale is faked: every
// connection to a node comes from the other node, with the grants the test
// chooses.

// fakeIdentity says every caller is peer.
type fakeIdentity struct {
	mu   sync.Mutex
	peer tailnet.Peer
}

func (f *fakeIdentity) WhoIs(context.Context, string) (tailnet.Peer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peer, nil
}

type node struct {
	name     string
	addr     netip.Addr // its fake Tailscale address
	mb       *mailbox.Mailbox
	srv      *httptest.Server
	identity *fakeIdentity
	up       atomic.Bool // false = the machine is asleep
}

type net2 struct {
	t     *testing.T
	nodes map[string]*node
}

type nodeOpts struct {
	agents   map[string]agent.Adapter
	giveUp   time.Duration
	doorbell mailbox.Doorbell
	wrap     func(http.Handler) http.Handler
}

func newNet(t *testing.T) *net2 {
	return &net2{t: t, nodes: map[string]*node{}}
}

// add starts a node. Its transport dials other nodes through the test's
// listeners, and fails while the target node is "asleep".
func (n *net2) add(name string, ip string, o nodeOpts) *node {
	t := n.t
	nd := &node{name: name, addr: netip.MustParseAddr(ip), identity: &fakeIdentity{}}
	nd.up.Store(true)
	n.nodes[name] = nd

	dialer := &net.Dialer{}
	hc := &http.Client{Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			host, _, _ := net.SplitHostPort(addr)
			for _, other := range n.nodes {
				if other.addr.String() == host {
					if !other.up.Load() {
						return nil, errors.New("connect: host is down")
					}
					return dialer.DialContext(ctx, "tcp", other.srv.Listener.Addr().String())
				}
			}
			return nil, errors.New("no route")
		},
	}}
	lookup := func(_ context.Context, name string) (netip.Addr, bool, error) {
		other, ok := n.nodes[name]
		if !ok {
			return netip.Addr{}, false, tailnet.ErrNoSuchNode
		}
		return other.addr, other.up.Load(), nil
	}
	mb, err := mailbox.Open(mailbox.Options{
		Path:        filepath.Join(t.TempDir(), name+".db"),
		Node:        name,
		Agents:      o.agents,
		Transport:   &Client{Lookup: lookup, HTTP: hc},
		Doorbell:    o.doorbell,
		GiveUpAfter: o.giveUp,
		RetryDelay:  20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	nd.mb = mb
	h := Handler(mb, nd.identity)
	if o.wrap != nil {
		h = o.wrap(h)
	}
	nd.srv = httptest.NewServer(h)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { mb.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
		nd.srv.Close()
		mb.Close()
	})
	return nd
}

// callerIs sets who nd sees as calling it, and that caller's grants.
func (nd *node) callerIs(peerNode string, grants ...tailnet.Grant) {
	nd.identity.mu.Lock()
	nd.identity.peer = tailnet.Peer{Node: peerNode, Grants: grants}
	nd.identity.mu.Unlock()
}

func (nd *node) send(t *testing.T, env envelope.Envelope) mailbox.Receipt {
	t.Helper()
	r, err := nd.mb.Send(env)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func inbox(t *testing.T, nd *node, agentName string) []mailbox.Item {
	t.Helper()
	items, err := nd.mb.Inbox(agentName, true)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func echoWorker() agent.Adapter {
	return agent.Func(func(_ context.Context, job agent.Job, report agent.Report) agent.Outcome {
		report.Thread("codex-thread")
		return agent.Outcome{State: envelope.Completed, Text: "echo: " + job.Message.Body.Text, ThreadID: "codex-thread"}
	})
}

// T4 in miniature: a Grok bot asks Codex on the iMac, and the Result goes back.
func TestRequestAcrossNodesAndResultBack(t *testing.T) {
	n := newNet(t)
	imac := n.add("imac", "100.64.0.1", nodeOpts{agents: map[string]agent.Adapter{"codex": echoWorker()}})
	grok := n.add("grok", "100.64.0.2", nodeOpts{agents: map[string]agent.Adapter{"tokdev": nil}})
	imac.callerIs("grok", tailnet.Grant{To: []string{"codex", "claude"}})
	grok.callerIs("imac", tailnet.Grant{To: []string{"*"}})

	r := grok.send(t, envelope.Envelope{Kind: envelope.Request, To: "imac/codex", FromAgent: "tokdev",
		FromThread: "tokdev", Body: envelope.Body{Text: "hello"}})
	if !r.Queued {
		t.Fatal("a message for another node should be queued in the outbox")
	}

	waitFor(t, "the result on grok", func() bool { return len(inbox(t, grok, "tokdev")) == 1 })
	res := inbox(t, grok, "tokdev")[0]
	if res.Message.Kind != envelope.Result || res.Message.ReplyTo != r.ID || res.Message.Body.Text != "echo: hello" {
		t.Fatalf("result = %+v", res.Message)
	}
	if res.From != "imac/codex" {
		t.Fatalf("result from %q; the receiver must name the sender by its Tailscale node", res.From)
	}
	if res.Message.ToThread != "tokdev" || res.Message.FromThread != "codex-thread" {
		t.Fatalf("threads: %q -> %q", res.Message.FromThread, res.Message.ToThread)
	}
	// The request was stored on imac as coming from grok/tokdev.
	req := inbox(t, imac, "codex")[0]
	if req.From != "grok/tokdev" {
		t.Fatalf("request from %q", req.From)
	}

	// Grok may now continue the Codex thread it was given...
	grok.send(t, envelope.Envelope{Kind: envelope.Request, To: "imac/codex", FromAgent: "tokdev",
		FromThread: "tokdev", ToThread: "codex-thread", Body: envelope.Body{Text: "again"}})
	waitFor(t, "the second result", func() bool { return len(inbox(t, grok, "tokdev")) == 2 })
	if got := inbox(t, grok, "tokdev")[1].Message.Body.State; got != envelope.Completed {
		t.Fatalf("second request: %s", got)
	}
}

func TestNoGrantMeansRefusedWithNotice(t *testing.T) {
	n := newNet(t)
	imac := n.add("imac", "100.64.0.1", nodeOpts{agents: map[string]agent.Adapter{"codex": echoWorker(), "claude": echoWorker()}})
	grok := n.add("grok", "100.64.0.2", nodeOpts{agents: map[string]agent.Adapter{"tokdev": nil}})
	imac.callerIs("grok", tailnet.Grant{To: []string{"claude"}}) // claude only

	grok.send(t, envelope.Envelope{Kind: envelope.Request, To: "imac/codex", FromAgent: "tokdev", Body: envelope.Body{Text: "x"}})
	waitFor(t, "the refusal notice", func() bool { return len(inbox(t, grok, "tokdev")) == 1 })
	notice := inbox(t, grok, "tokdev")[0]
	if notice.Message.Kind != envelope.Notice || notice.From != "grok/baf" ||
		!strings.Contains(notice.Message.Body.Text, "may not message imac/codex") {
		t.Fatalf("notice = %s from %s: %q", notice.Message.Kind, notice.From, notice.Message.Body.Text)
	}
	if len(inbox(t, imac, "codex")) != 0 {
		t.Fatal("codex got a message the policy does not allow")
	}
	out, _ := grok.mb.Outbox(true, 10)
	if len(out) != 1 || out[0].State != "failed" {
		t.Fatalf("outbox = %+v", out)
	}
}

func TestSleepingReceiverGetsMessagesInOrderWhenItWakes(t *testing.T) {
	n := newNet(t)
	var mu sync.Mutex
	var got []string
	recorder := agent.Func(func(_ context.Context, job agent.Job, _ agent.Report) agent.Outcome {
		mu.Lock()
		got = append(got, job.Message.Body.Text)
		mu.Unlock()
		return agent.Outcome{State: envelope.Completed}
	})
	imac := n.add("imac", "100.64.0.1", nodeOpts{agents: map[string]agent.Adapter{"me": nil}})
	grok := n.add("grok", "100.64.0.2", nodeOpts{agents: map[string]agent.Adapter{"tokdev": recorder}})
	grok.callerIs("imac", tailnet.Grant{To: []string{"*"}})
	grok.up.Store(false) // grokbot is asleep

	for _, text := range []string{"one", "two", "three"} {
		imac.send(t, envelope.Envelope{Kind: envelope.Notice, To: "grok/tokdev", FromAgent: "me",
			FromThread: "t", Body: envelope.Body{Text: text}})
	}
	waitFor(t, "a few failed tries", func() bool {
		out, _ := imac.mb.Outbox(false, 10)
		return len(out) == 3 && out[2].Attempts >= 2 // out is newest first
	})
	out, _ := imac.mb.Outbox(false, 10)
	if out[0].Attempts != 0 || out[1].Attempts != 0 {
		t.Fatalf("later messages were tried before the first got through: %+v", out)
	}

	grok.up.Store(true)
	waitFor(t, "all three delivered", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 3
	})
	if strings.Join(got, ",") != "one,two,three" {
		t.Fatalf("order = %v", got)
	}
}

func TestGiveUpTellsSenderAndFailsLaterMessages(t *testing.T) {
	n := newNet(t)
	imac := n.add("imac", "100.64.0.1", nodeOpts{agents: map[string]agent.Adapter{"me": nil}, giveUp: 300 * time.Millisecond})
	grok := n.add("grok", "100.64.0.2", nodeOpts{agents: map[string]agent.Adapter{"tokdev": nil}})
	grok.up.Store(false) // and it never wakes

	first := imac.send(t, envelope.Envelope{Kind: envelope.Request, To: "grok/tokdev", FromAgent: "me", FromThread: "t",
		Body: envelope.Body{Text: "first"}})
	imac.send(t, envelope.Envelope{Kind: envelope.Request, To: "grok/tokdev", FromAgent: "me", FromThread: "t",
		Body: envelope.Body{Text: "second"}})

	waitFor(t, "two failure notices", func() bool { return len(inbox(t, imac, "me")) == 2 })
	notices := inbox(t, imac, "me")
	if !strings.Contains(notices[0].Message.Body.Text, "could not be confirmed") ||
		!strings.Contains(notices[0].Message.Body.Text, first.ID) {
		t.Fatalf("first notice: %q", notices[0].Message.Body.Text)
	}
	if !strings.Contains(notices[1].Message.Body.Text, "was not sent, because the earlier message "+first.ID) {
		t.Fatalf("second notice: %q", notices[1].Message.Body.Text)
	}
	if notices[0].Message.ToThread != "t" {
		t.Fatal("a failure notice must go back into the thread the message came from")
	}
}

func TestDoorbellRingsOnFirstPushOnly(t *testing.T) {
	var rings atomic.Int32
	var gotAuth, gotBody atomic.Value
	bell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rings.Add(1)
		gotAuth.Store(r.Header.Get("Authorization"))
		b, _ := io.ReadAll(r.Body)
		gotBody.Store(string(b))
	}))
	defer bell.Close()

	n := newNet(t)
	doorbells := &Doorbells{From: "imac", Bells: map[string]Bell{"grok": {URL: bell.URL, Key: "secret"}}}
	imac := n.add("imac", "100.64.0.1", nodeOpts{agents: map[string]agent.Adapter{"me": nil}, doorbell: doorbells})
	grok := n.add("grok", "100.64.0.2", nodeOpts{agents: map[string]agent.Adapter{"tokdev": nil}})
	grok.callerIs("imac", tailnet.Grant{To: []string{"*"}})
	grok.up.Store(false)

	imac.send(t, envelope.Envelope{Kind: envelope.Notice, To: "grok/tokdev", FromAgent: "me", Body: envelope.Body{Text: "wake"}})
	waitFor(t, "retries", func() bool {
		out, _ := imac.mb.Outbox(false, 1)
		return len(out) == 1 && out[0].Attempts >= 3
	})
	grok.up.Store(true)
	waitFor(t, "delivery", func() bool { return len(inbox(t, grok, "tokdev")) == 1 })

	if n := rings.Load(); n != 1 {
		t.Fatalf("rang %d times; retries must not ring again", n)
	}
	if gotAuth.Load() != "Bearer secret" {
		t.Fatalf("auth header %q", gotAuth.Load())
	}
	// The ring names the message and its sender, never its content.
	body := gotBody.Load().(string)
	if !strings.Contains(body, `"from":"imac/me","to":"grok/tokdev","kind":"notice"`) || strings.Contains(body, "wake") {
		t.Fatalf("ring body %s", body)
	}
}

func TestUnknownMachineIsRefusedAtOnce(t *testing.T) {
	n := newNet(t)
	imac := n.add("imac", "100.64.0.1", nodeOpts{agents: map[string]agent.Adapter{"me": nil}})
	_, err := imac.mb.Send(envelope.Envelope{Kind: envelope.Notice, To: "grokbt/tokdev", FromAgent: "me", Body: envelope.Body{Text: "x"}})
	var rej *mailbox.Rejection
	if !errors.As(err, &rej) || !strings.Contains(rej.Reason, "no machine") {
		t.Fatalf("err = %v", err)
	}
}

func TestPeerMayMessage(t *testing.T) {
	p := tailnet.Peer{Grants: []tailnet.Grant{{To: []string{"codex"}}, {Relay: true}}}
	if !p.MayMessage("codex") || p.MayMessage("claude") {
		t.Fatal("specific grant")
	}
	if !(tailnet.Peer{Grants: []tailnet.Grant{{To: []string{"*"}}}}).MayMessage("anyone") {
		t.Fatal("wildcard grant")
	}
	if (tailnet.Peer{}).MayMessage("codex") {
		t.Fatal("no grant must mean no access")
	}
}

func status(t *testing.T, nd *node, id string) mailbox.SentStatus {
	t.Helper()
	s, err := nd.mb.SentStatus(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A Grok bot follows its request to Codex on the iMac: handed over, what
// Codex is doing there, then the Result. Only the sending node may ask, and
// only while the policy still lets it message Codex.
func TestStatusAcrossNodes(t *testing.T) {
	n := newNet(t)
	release := make(chan struct{})
	worker := agent.Func(func(ctx context.Context, job agent.Job, report agent.Report) agent.Outcome {
		report.Thread("codex-thread")
		report.Doing("running: go test ./...")
		select {
		case <-release:
		case <-ctx.Done():
		}
		return agent.Outcome{State: envelope.Completed, Text: "ok", ThreadID: "codex-thread"}
	})
	imac := n.add("imac", "100.64.0.1", nodeOpts{agents: map[string]agent.Adapter{"codex": worker}})
	grok := n.add("grok", "100.64.0.2", nodeOpts{agents: map[string]agent.Adapter{"tokdev": nil}})
	imac.callerIs("grok", tailnet.Grant{To: []string{"codex"}})
	grok.callerIs("imac", tailnet.Grant{To: []string{"*"}})

	r := grok.send(t, envelope.Envelope{Kind: envelope.Request, To: "imac/codex", FromAgent: "tokdev",
		Body: envelope.Body{Text: "run the tests"}})
	var s mailbox.SentStatus
	waitFor(t, "codex to say what it is doing", func() bool {
		s = status(t, grok, r.ID)
		return s.Receiver != nil && s.Receiver.Run != nil && s.Receiver.Run.Doing != ""
	})
	if s.Handover == nil || s.Handover.State != "handed" || s.Handover.DoneAt == nil {
		t.Fatalf("handover = %+v", s.Handover)
	}
	if p := s.Receiver; p.To != "imac/codex" || p.State != "running" || p.Run.Doing != "running: go test ./..." {
		t.Fatalf("progress = %+v, run = %+v", p, p.Run)
	}

	imac.callerIs("mac2", tailnet.Grant{To: []string{"*"}})
	if s := status(t, grok, r.ID); s.Receiver != nil || !strings.Contains(s.ReceiverError, "imac has no message") {
		t.Fatalf("another node asking: %+v", s)
	}
	imac.callerIs("grok") // the grant was removed
	if s := status(t, grok, r.ID); s.Receiver != nil || !strings.Contains(s.ReceiverError, "may no longer message imac/codex") {
		t.Fatalf("without a grant: %+v", s)
	}
	imac.callerIs("grok", tailnet.Grant{To: []string{"codex"}})
	imac.up.Store(false)
	if s := status(t, grok, r.ID); s.Receiver != nil || !strings.Contains(s.ReceiverError, "host is down") {
		t.Fatalf("imac asleep: %+v", s)
	}
	imac.up.Store(true)

	close(release)
	waitFor(t, "the result", func() bool { return status(t, grok, r.ID).Result != nil })
	s = status(t, grok, r.ID)
	if s.Result.State != "completed" || s.Receiver != nil || s.ReceiverError != "" {
		t.Fatalf("once the Result is back, imac isn't asked: %+v", s)
	}
}

func TestOlderReceiverCantReportProgress(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler()) // a baf from before /v1/progress
	defer srv.Close()
	ap := netip.MustParseAddrPort(srv.Listener.Addr().String())
	c := &Client{
		Lookup: func(context.Context, string) (netip.Addr, bool, error) { return ap.Addr(), true, nil },
		Port:   int(ap.Port()),
	}
	_, err := c.Progress(context.Background(), "grokbot", "01X")
	if err == nil || !strings.Contains(err.Error(), "grokbot runs an older baf") {
		t.Fatalf("err = %v", err)
	}
}

func addFile(t *testing.T, nd *node, name, contents string) envelope.File {
	t.Helper()
	f, err := nd.mb.AddFile(name, strings.NewReader(contents))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestFilesAcrossNodes(t *testing.T) {
	for _, sleeping := range []bool{false, true} {
		name := "awake"
		if sleeping {
			name = "asleep"
		}
		t.Run(name, func(t *testing.T) {
			n := newNet(t)
			var uploads atomic.Int32
			imac := n.add("imac", "100.64.0.1", nodeOpts{agents: map[string]agent.Adapter{"me": nil}, wrap: func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasPrefix(r.URL.Path, "/v1/files/") {
						uploads.Add(1)
					}
					next.ServeHTTP(w, r)
				})
			}})
			grok := n.add("grok", "100.64.0.2", nodeOpts{agents: map[string]agent.Adapter{"tokdev": nil}})
			imac.callerIs("grok", tailnet.Grant{To: []string{"me"}})
			files := []envelope.File{addFile(t, grok, "report.pdf", "report bytes"), addFile(t, grok, "empty.txt", "")}
			// Two names may refer to the same bytes, but each hash travels only once.
			alias := files[0]
			alias.Name = "report-copy.pdf"
			files = append(files, alias)
			imac.up.Store(!sleeping)
			send := func() {
				grok.send(t, envelope.Envelope{Kind: envelope.Notice, To: "imac/me", FromAgent: "tokdev", Body: envelope.Body{Text: "files", Files: files}})
			}
			send()
			if sleeping {
				waitFor(t, "file retries", func() bool {
					out, _ := grok.mb.Outbox(false, 10)
					return len(out) == 1 && out[0].Attempts >= 2 && strings.Contains(out[0].LastError, "handing over report.pdf")
				})
				if len(inbox(t, imac, "me")) != 0 {
					t.Fatal("message arrived while asleep")
				}
				imac.up.Store(true)
			}
			waitFor(t, "files and notice", func() bool { return len(inbox(t, imac, "me")) == 1 })
			send()
			waitFor(t, "reused files", func() bool { return len(inbox(t, imac, "me")) == 2 })
			for _, item := range inbox(t, imac, "me") {
				if len(item.Files) != len(files) {
					t.Fatalf("files = %+v", item.Files)
				}
				for i, f := range item.Files {
					b, err := os.ReadFile(f.Path)
					want := "report bytes"
					if i == 1 {
						want = ""
					}
					if err != nil || string(b) != want || f.File != files[i] {
						t.Fatalf("file = %+v, bytes = %q, err = %v", f, b, err)
					}
				}
			}
			if uploads.Load() != 4 {
				t.Fatalf("uploads = %d, want 4", uploads.Load())
			}
		})
	}
}

func TestRequestWithReadableFiles(t *testing.T) {
	n := newNet(t)
	worker := agent.Func(func(_ context.Context, job agent.Job, report agent.Report) agent.Outcome {
		if len(job.Files) != 1 {
			return agent.Outcome{State: envelope.Failed, Text: "missing file"}
		}
		b, err := os.ReadFile(job.Files[0].Path)
		if err != nil {
			return agent.Outcome{State: envelope.Failed, Text: err.Error()}
		}
		report.Thread("codex-thread")
		return agent.Outcome{State: envelope.Completed, Text: "echo: " + string(b), ThreadID: "codex-thread"}
	})
	imac := n.add("imac", "100.64.0.1", nodeOpts{agents: map[string]agent.Adapter{"codex": worker}})
	grok := n.add("grok", "100.64.0.2", nodeOpts{agents: map[string]agent.Adapter{"tokdev": nil}})
	imac.callerIs("grok", tailnet.Grant{To: []string{"codex"}})
	grok.callerIs("imac", tailnet.Grant{To: []string{"tokdev"}})
	f := addFile(t, grok, "report.pdf", "hello from file")
	r := grok.send(t, envelope.Envelope{Kind: envelope.Request, To: "imac/codex", FromAgent: "tokdev", Body: envelope.Body{Text: "read", Files: []envelope.File{f}}})
	waitFor(t, "file result", func() bool { return len(inbox(t, grok, "tokdev")) == 1 })
	result := inbox(t, grok, "tokdev")[0].Message
	if result.Kind != envelope.Result || result.ReplyTo != r.ID || result.Body.State != envelope.Completed || result.Body.Text != "echo: hello from file" {
		t.Fatalf("result = %+v", result)
	}
}

func TestFileRefusalNotice(t *testing.T) {
	for _, older := range []bool{false, true} {
		name := "no grant"
		if older {
			name = "older receiver"
		}
		t.Run(name, func(t *testing.T) {
			n := newNet(t)
			imac := n.add("imac", "100.64.0.1", nodeOpts{agents: map[string]agent.Adapter{"me": nil}, wrap: func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if older && strings.HasPrefix(r.URL.Path, "/v1/files/") {
						http.NotFound(w, r)
						return
					}
					next.ServeHTTP(w, r)
				})
			}})
			grok := n.add("grok", "100.64.0.2", nodeOpts{agents: map[string]agent.Adapter{"tokdev": nil}})
			imac.callerIs("grok", tailnet.Grant{To: []string{"codex"}})
			if older {
				imac.callerIs("grok", tailnet.Grant{To: []string{"me"}})
			}
			f := addFile(t, grok, "report.pdf", "report")
			grok.send(t, envelope.Envelope{Kind: envelope.Notice, To: "imac/me", FromAgent: "tokdev", Body: envelope.Body{Text: "read", Files: []envelope.File{f}}})
			waitFor(t, "file refusal", func() bool { return len(inbox(t, grok, "tokdev")) == 1 })
			notice := inbox(t, grok, "tokdev")[0].Message
			reason := "may not message imac/me"
			if older {
				reason = "imac runs an older baf that can't receive files; update it there"
			}
			if notice.Kind != envelope.Notice || !strings.Contains(notice.Body.Text, "refused file report.pdf") || !strings.Contains(notice.Body.Text, reason) {
				t.Fatalf("notice = %+v", notice)
			}
			if len(inbox(t, imac, "me")) != 0 {
				t.Fatal("refused message stored")
			}
			out, err := grok.mb.Outbox(true, 10)
			if err != nil || len(out) != 1 || out[0].State != "failed" {
				t.Fatalf("outbox = %+v, err = %v", out, err)
			}
		})
	}
}

type unreadableBody struct{ read bool }

func (b *unreadableBody) Read([]byte) (int, error) {
	b.read = true
	return 0, errors.New("must not read")
}
func (b *unreadableBody) Close() error { return nil }

func TestFileHandlerChecks(t *testing.T) {
	n := newNet(t)
	imac := n.add("imac", "100.64.0.1", nodeOpts{agents: map[string]agent.Adapter{"me": nil}})
	for _, tc := range []struct {
		name   string
		size   int64
		grant  bool
		to     string
		code   int
		noRead bool
	}{
		{"wrong hash", 3, true, "me", 422, false},
		{"no grant", 3, false, "me", 403, true},
		{"too large", envelope.MaxFileSize + 1, true, "me", 413, true},
		{"unknown length", -1, true, "me", 411, true},
		{"missing length", 0, true, "me", 411, true},
		{"bad agent", 3, true, "bad/agent", 400, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			imac.callerIs("grok")
			if tc.grant {
				imac.callerIs("grok", tailnet.Grant{To: []string{"*"}})
			}
			req := httptest.NewRequest("PUT", "/v1/files/"+strings.Repeat("0", 64)+"?to="+tc.to, strings.NewReader("abc"))
			req.ContentLength = tc.size
			body := &unreadableBody{}
			if tc.noRead {
				req.Body = body
			}
			rec := httptest.NewRecorder()
			Handler(imac.mb, imac.identity).ServeHTTP(rec, req)
			var fr fileResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &fr); err != nil {
				t.Fatal(err)
			}
			if rec.Code != tc.code || !fr.Rejected || fr.Error == "" || body.read {
				t.Fatalf("code=%d response=%+v read=%v", rec.Code, fr, body.read)
			}
		})
	}
}
