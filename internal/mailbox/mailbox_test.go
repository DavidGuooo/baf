package mailbox

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DavidGuooo/baf/internal/agent"
	"github.com/DavidGuooo/baf/internal/envelope"
)

// fakeAgent records each job it gets and answers with reply. Each new thread
// it starts is named t1, t2, ...
type fakeAgent struct {
	mu      sync.Mutex
	jobs    []agent.Job
	threads int
	reply   func(ctx context.Context, job agent.Job) agent.Outcome
}

func (f *fakeAgent) Run(ctx context.Context, job agent.Job, onThread func(string)) agent.Outcome {
	f.mu.Lock()
	f.jobs = append(f.jobs, job)
	thread := job.ResumeThread
	if thread == "" {
		f.threads++
		thread = "t" + string(rune('0'+f.threads))
	}
	f.mu.Unlock()
	onThread(thread)
	out := agent.Outcome{State: envelope.Completed, Text: "done: " + job.Message.Body.Text}
	if f.reply != nil {
		out = f.reply(ctx, job)
	}
	out.ThreadID = thread
	return out
}

func (f *fakeAgent) jobCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.jobs)
}

type harness struct {
	t      *testing.T
	path   string
	worker *fakeAgent
	clock  *time.Time
	mb     *Mailbox
	cancel context.CancelFunc
	done   chan struct{}
}

// newHarness opens a mailbox on node "box" with a fake agent "worker" and
// inbox-only agents "me" and "other", and starts its dispatcher.
func newHarness(t *testing.T) *harness {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	h := &harness{t: t, path: filepath.Join(t.TempDir(), "mailbox.db"), worker: &fakeAgent{}, clock: &now}
	h.open()
	t.Cleanup(h.stop)
	return h
}

func (h *harness) open() {
	h.t.Helper()
	mb, err := Open(Options{
		Path:   h.path,
		Node:   "box",
		Agents: map[string]agent.Adapter{"worker": h.worker, "me": nil, "other": nil},
		Now:    func() time.Time { return *h.clock },
	})
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.mb, h.cancel, h.done = mb, cancel, make(chan struct{})
	go func() { mb.Run(ctx); close(h.done) }()
}

// stop cancels the dispatcher, waits for it, and closes the database.
func (h *harness) stop() {
	if h.mb == nil {
		return
	}
	h.cancel()
	<-h.done
	h.mb.Close()
	h.mb = nil
}

func (h *harness) send(env envelope.Envelope) (Receipt, error) {
	h.t.Helper()
	if env.FromAgent == "" {
		env.FromAgent = "me"
	}
	if env.To == "" {
		env.To = "box/worker"
	}
	return h.mb.Send(env)
}

// waitInbox waits until agentName has at least n messages, read or not.
func (h *harness) waitInbox(agentName string, n int) []Item {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		items, err := h.mb.Inbox(agentName, true)
		if err != nil {
			h.t.Fatal(err)
		}
		if len(items) >= n {
			return items
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("%s has %d messages, want %d", agentName, len(items), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRequestGetsAutomaticResult(t *testing.T) {
	h := newHarness(t)
	r, err := h.send(envelope.Envelope{Kind: envelope.Request, FromThread: "my-thread", Body: envelope.Body{Text: "hi"}})
	if err != nil {
		t.Fatal(err)
	}

	res := h.waitInbox("me", 1)[0].Message
	if res.Kind != envelope.Result || res.ReplyTo != r.ID {
		t.Fatalf("got %s replying to %q, want a result for %s", res.Kind, res.ReplyTo, r.ID)
	}
	if res.Body.State != envelope.Completed || res.Body.Text != "done: hi" {
		t.Fatalf("result = %s %q", res.Body.State, res.Body.Text)
	}
	// A reply swaps the thread ids: the worker's new thread, and ours back.
	if res.FromThread != "t1" || res.ToThread != "my-thread" {
		t.Fatalf("threads: from %q to %q", res.FromThread, res.ToThread)
	}
	if res.FromAgent != "worker" {
		t.Fatalf("from_agent %q", res.FromAgent)
	}
}

func TestNoticeGetsNoResult(t *testing.T) {
	h := newHarness(t)
	if _, err := h.send(envelope.Envelope{Kind: envelope.Notice, Body: envelope.Body{Text: "fyi"}}); err != nil {
		t.Fatal(err)
	}
	// Then a request, whose Result proves the notice's run already finished.
	if _, err := h.send(envelope.Envelope{Kind: envelope.Request, Body: envelope.Body{Text: "q"}}); err != nil {
		t.Fatal(err)
	}
	h.waitInbox("me", 1)
	time.Sleep(50 * time.Millisecond)
	items, _ := h.mb.Inbox("me", true)
	if len(items) != 1 || items[0].Message.Body.Text != "done: q" {
		t.Fatalf("me's inbox: %+v", items)
	}
	if h.worker.jobCount() != 2 {
		t.Fatalf("worker ran %d times, want 2 (a notice still reaches the agent)", h.worker.jobCount())
	}
}

func TestRetryIsStoredOnce(t *testing.T) {
	h := newHarness(t)
	env := envelope.Envelope{ID: envelope.NewID(), Kind: envelope.Request, Body: envelope.Body{Text: "once"}}
	first, err := h.send(env)
	if err != nil {
		t.Fatal(err)
	}
	again, err := h.send(env)
	if err != nil {
		t.Fatal(err)
	}
	if first.Duplicate || !again.Duplicate || !again.StoredAt.Equal(first.StoredAt) {
		t.Fatalf("receipts: %+v then %+v", first, again)
	}
	h.waitInbox("me", 1)
	time.Sleep(50 * time.Millisecond)
	if n := h.worker.jobCount(); n != 1 {
		t.Fatalf("worker ran %d times", n)
	}
}

func TestContinueOnlySharedThreads(t *testing.T) {
	h := newHarness(t)

	// A thread the worker never shared with me is refused, not started fresh.
	_, err := h.send(envelope.Envelope{Kind: envelope.Request, ToThread: "t1", Body: envelope.Body{Text: "sneak"}})
	var rej *Rejection
	if !errors.As(err, &rej) || !strings.Contains(rej.Reason, "never shared") {
		t.Fatalf("err = %v, want a never-shared rejection", err)
	}

	// The worker's Result shares thread t1 with me, so now I may continue it.
	h.send(envelope.Envelope{Kind: envelope.Request, Body: envelope.Body{Text: "start"}})
	thread := h.waitInbox("me", 1)[0].Message.FromThread
	if _, err := h.send(envelope.Envelope{Kind: envelope.Request, ToThread: thread, Body: envelope.Body{Text: "more"}}); err != nil {
		t.Fatal(err)
	}
	h.waitInbox("me", 2)
	if got := h.worker.jobs[1].ResumeThread; got != thread {
		t.Fatalf("second run resumed %q, want %q", got, thread)
	}

	// Another sender may not use the thread the worker shared with me.
	_, err = h.send(envelope.Envelope{Kind: envelope.Request, FromAgent: "other", ToThread: thread, Body: envelope.Body{Text: "x"}})
	if !errors.As(err, &rej) {
		t.Fatalf("other sender: err = %v, want rejection", err)
	}
}

func TestSharedThreadExpires(t *testing.T) {
	h := newHarness(t)
	h.send(envelope.Envelope{Kind: envelope.Request, Body: envelope.Body{Text: "start"}})
	thread := h.waitInbox("me", 1)[0].Message.FromThread

	*h.clock = h.clock.Add(29 * 24 * time.Hour) // still inside 30 days
	if _, err := h.send(envelope.Envelope{Kind: envelope.Request, ToThread: thread, Body: envelope.Body{Text: "a"}}); err != nil {
		t.Fatal(err)
	}
	h.waitInbox("me", 2)

	*h.clock = h.clock.Add(31 * 24 * time.Hour) // 31 days after the last use
	_, err := h.send(envelope.Envelope{Kind: envelope.Request, ToThread: thread, Body: envelope.Body{Text: "b"}})
	var rej *Rejection
	if !errors.As(err, &rej) || !strings.Contains(rej.Reason, "expired") {
		t.Fatalf("err = %v, want an expiry rejection", err)
	}
}

func TestOneRunPerThreadAtATime(t *testing.T) {
	h := newHarness(t)
	h.send(envelope.Envelope{Kind: envelope.Request, Body: envelope.Body{Text: "start"}})
	thread := h.waitInbox("me", 1)[0].Message.FromThread

	release := make(chan struct{})
	var mu sync.Mutex
	running, maxRunning := 0, 0
	h.worker.reply = func(ctx context.Context, job agent.Job) agent.Outcome {
		mu.Lock()
		running++
		maxRunning = max(maxRunning, running)
		mu.Unlock()
		<-release
		mu.Lock()
		running--
		mu.Unlock()
		return agent.Outcome{State: envelope.Completed, Text: job.Message.Body.Text}
	}
	for _, text := range []string{"a", "b", "c"} {
		if _, err := h.send(envelope.Envelope{Kind: envelope.Request, ToThread: thread, Body: envelope.Body{Text: text}}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	items := h.waitInbox("me", 4)
	if maxRunning != 1 {
		t.Fatalf("%d runs on one thread at once", maxRunning)
	}
	// Same thread, so the requests ran in the order they arrived.
	for i, want := range []string{"a", "b", "c"} {
		if got := items[i+1].Message.Body.Text; got != want {
			t.Fatalf("result %d = %q, want %q", i, got, want)
		}
	}
}

func TestStopMarksRunFailed(t *testing.T) {
	h := newHarness(t)
	started := make(chan struct{})
	h.worker.reply = func(ctx context.Context, job agent.Job) agent.Outcome {
		close(started)
		<-ctx.Done() // like a real agent process being stopped
		return agent.Outcome{State: envelope.Failed, Text: "killed"}
	}
	h.send(envelope.Envelope{Kind: envelope.Request, Body: envelope.Body{Text: "long job"}})
	<-started
	h.stop()

	h.worker.reply = nil
	h.open()
	res := h.waitInbox("me", 1)[0].Message
	if res.Body.State != envelope.Failed || !strings.Contains(res.Body.Text, "shutting down or restarting") {
		t.Fatalf("result = %s %q", res.Body.State, res.Body.Text)
	}
}

func TestCrashLeavesOutcomeUnknownAndNeverReruns(t *testing.T) {
	h := newHarness(t)
	// Simulate a crash: a message claimed and its run recorded, but the
	// mailbox never saw the run end.
	h.stop()
	h.open()
	h.cancel() // keep the dispatcher out of the way
	<-h.done
	if _, err := h.mb.Send(envelope.Envelope{Kind: envelope.Request, FromAgent: "me", To: "box/worker", Body: envelope.Body{Text: "x"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.mb.claim(); err != nil {
		t.Fatal(err)
	}
	h.mb.Close()
	h.mb = nil

	h.open() // the restart
	res := h.waitInbox("me", 1)[0].Message
	if res.Body.State != envelope.Unknown {
		t.Fatalf("state = %s, want unknown", res.Body.State)
	}
	time.Sleep(50 * time.Millisecond)
	if n := h.worker.jobCount(); n != 0 {
		t.Fatalf("worker ran %d times after a crash; it must never re-run", n)
	}
	runs, _ := h.mb.Runs(10)
	if len(runs) != 1 || runs[0].State != "unknown" {
		t.Fatalf("runs = %+v", runs)
	}
}

func TestRejectsUnknownAgentsAndRemoteForNow(t *testing.T) {
	h := newHarness(t)
	var rej *Rejection
	if _, err := h.send(envelope.Envelope{Kind: envelope.Notice, To: "box/nobody", Body: envelope.Body{Text: "x"}}); !errors.As(err, &rej) {
		t.Fatalf("unknown receiver: %v", err)
	}
	if _, err := h.send(envelope.Envelope{Kind: envelope.Notice, FromAgent: "ghost", Body: envelope.Body{Text: "x"}}); !errors.As(err, &rej) {
		t.Fatalf("unknown sender: %v", err)
	}
	if _, err := h.send(envelope.Envelope{Kind: envelope.Notice, To: "elsewhere/worker", Body: envelope.Body{Text: "x"}}); !errors.Is(err, ErrRemoteNotSupported) {
		t.Fatalf("remote: %v", err)
	}
}

func TestReadMarksRead(t *testing.T) {
	h := newHarness(t)
	h.send(envelope.Envelope{Kind: envelope.Request, Body: envelope.Body{Text: "q"}})
	id := h.waitInbox("me", 1)[0].Message.ID
	if _, err := h.mb.Read("me", id); err != nil {
		t.Fatal(err)
	}
	unread, _ := h.mb.Inbox("me", false)
	if len(unread) != 0 {
		t.Fatalf("still unread: %+v", unread)
	}
	if _, err := h.mb.Read("me", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}
