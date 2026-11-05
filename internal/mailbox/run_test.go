package mailbox

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DavidGuooo/baf/internal/agent"
	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/DavidGuooo/baf/internal/runfolder"
	"github.com/DavidGuooo/baf/internal/tailnet"
)

func runMailbox(t *testing.T, worker agent.Adapter, choose func(string) (string, error)) *Mailbox {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mb, err := Open(Options{Path: filepath.Join(home, "mail.db"), Node: "box", UserHome: home,
		Agents: map[string]agent.Adapter{"worker": worker, "codex": agent.Codex{}, "me": nil}, NewThreadDir: choose,
		BafBinary: "/test/baf", BafHome: "/test/home", RunEnv: func(string) []string {
			return []string{"BAF_AGENT=worker", "BAF_THREAD=inherited", "BAF_HOME=/test/home", "PATH=/test/bin"}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mb.Close() })
	return mb
}

func runMessage() envelope.Envelope {
	return envelope.Envelope{V: envelope.Version, ID: envelope.NewID(), Kind: envelope.Notice, To: "box/worker", FromAgent: "me"}
}

func executeOne(t *testing.T, mb *Mailbox) claimed {
	t.Helper()
	claims, err := mb.claim()
	if err != nil || len(claims) != 1 {
		t.Fatalf("claims=%v error=%v", claims, err)
	}
	mb.execute(context.Background(), claims[0])
	return claims[0]
}

func TestRunFolderStoredAndReused(t *testing.T) {
	worker := &fakeAgent{}
	scratchCalls := 0
	mb := runMailbox(t, worker, func(string) (string, error) { scratchCalls++; return "", nil })
	env := runMessage()
	env.Run = &envelope.Run{Dir: "projects/new"}
	if _, err := mb.Send(env); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(mb.userHome, "projects", "new")
	worker.reply = func(_ context.Context, job agent.Job) agent.Outcome {
		var dir string
		if err := mb.db.QueryRow(`SELECT dir FROM runs WHERE state='running' ORDER BY id DESC LIMIT 1`).Scan(&dir); err != nil || dir != job.Dir {
			t.Errorf("folder not recorded before run: dir=%q job=%q error=%v", dir, job.Dir, err)
		}
		return agent.Outcome{State: envelope.Completed}
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("created before run: %v", err)
	}
	c := executeOne(t, mb)
	if worker.jobs[0].Dir != want || scratchCalls != 0 {
		t.Fatalf("job=%+v scratch calls=%d", worker.jobs[0], scratchCalls)
	}
	var recorded string
	if err := mb.db.QueryRow(`SELECT dir FROM runs WHERE id=?`, c.runID).Scan(&recorded); err != nil || recorded != want {
		t.Fatalf("recorded=%q error=%v", recorded, err)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("missing folder: %v", err)
	}
	if err := os.Remove(want); err != nil {
		t.Fatal(err)
	}
	follow := runMessage()
	follow.ToThread = "t1"
	if _, err := mb.Send(follow); err != nil {
		t.Fatal(err)
	}
	executeOne(t, mb)
	if worker.jobs[1].Dir != want || scratchCalls != 0 {
		t.Fatalf("followup dir=%q calls=%d", worker.jobs[1].Dir, scratchCalls)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatal("folder not recreated:", err)
	}
	// Legacy records and an unknown old thread both retain adapter defaults.
	if _, err := mb.db.Exec(`UPDATE runs SET dir=''`); err != nil {
		t.Fatal(err)
	}
	for _, thread := range []string{"t1", "legacy"} {
		follow.ID = envelope.NewID()
		follow.ToThread = thread
		if _, err := mb.Send(follow); err != nil {
			t.Fatal(err)
		}
		executeOne(t, mb)
		if job := worker.jobs[len(worker.jobs)-1]; job.Dir != "" {
			t.Fatalf("legacy dir=%q", job.Dir)
		}
	}
}

func TestScratchChoiceAndPreparationFailure(t *testing.T) {
	scratch, err := runfolder.Resolve(filepath.Join(t.TempDir(), "scratch"), "/")
	if err != nil {
		t.Fatal(err)
	}
	worker := &fakeAgent{}
	mb := runMailbox(t, worker, func(string) (string, error) { return scratch, nil })
	env := runMessage()
	if _, err := mb.Send(env); err != nil {
		t.Fatal(err)
	}
	executeOne(t, mb)
	if worker.jobs[0].Dir != scratch {
		t.Fatalf("dir=%s", worker.jobs[0].Dir)
	}
	mb.newThreadDir = func(string) (string, error) { return "", errors.New("no random bytes") }
	env.ID = envelope.NewID()
	if _, err := mb.Send(env); err != nil {
		t.Fatal(err)
	}
	c := executeOne(t, mb)
	var state, detail string
	if err := mb.db.QueryRow(`SELECT state, detail FROM runs WHERE id=?`, c.runID).Scan(&state, &detail); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || !strings.Contains(detail, "no random bytes") || len(worker.jobs) != 1 {
		t.Fatalf("state=%s detail=%s jobs=%d", state, detail, len(worker.jobs))
	}
}

func TestPushRunPermissionsAndStoredResolution(t *testing.T) {
	worker := &fakeAgent{}
	mb := runMailbox(t, worker, nil)
	peer := tailnet.Peer{Node: "grokbot", Grants: []tailnet.Grant{{To: []string{"worker"}}}}
	env := runMessage()
	env.Run = &envelope.Run{Dir: "repo/new"}
	_, err := mb.AcceptPush(context.Background(), env, peer)
	wantDoorRejection(t, err, "dirs")
	if !strings.Contains(err.Error(), "grokbot") {
		t.Fatal(err)
	}
	peer.Grants[0].Dirs = []string{"repo"}
	if _, err := mb.AcceptPush(context.Background(), env, peer); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(mb.userHome, "repo", "new")
	var dir string
	if err := mb.db.QueryRow(`SELECT run_dir FROM messages WHERE id=?`, env.ID).Scan(&dir); err != nil || dir != want {
		t.Fatalf("dir=%q error=%v", dir, err)
	}
	items, err := mb.Inbox("worker", false, 10)
	if err != nil || items[0].Message.Run.Dir != "repo/new" {
		t.Fatalf("envelope changed: %+v %v", items, err)
	}
	executeOne(t, mb)
	if worker.jobs[0].Dir != want {
		t.Fatalf("ran %q", worker.jobs[0].Dir)
	}
	// A local sender is unrestricted even outside the receiver's home.
	env = runMessage()
	env.Run = &envelope.Run{Dir: filepath.Join(t.TempDir(), "outside")}
	if _, err := mb.Send(env); err != nil {
		t.Fatal(err)
	}
	executeOne(t, mb)
}

func TestStoredFolderDoesNotFollowChangedSymlinks(t *testing.T) {
	worker := &fakeAgent{}
	mb := runMailbox(t, worker, nil)
	root := filepath.Join(mb.userHome, "repo")
	outside := filepath.Join(mb.userHome, "outside")
	for _, dir := range []string{root, outside} {
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(mb.userHome, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	env := runMessage()
	env.Run = &envelope.Run{Dir: alias + "/new"}
	if _, err := mb.Send(env); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, alias); err != nil {
		t.Fatal(err)
	}
	executeOne(t, mb)
	if worker.jobs[0].Dir != root+"/new" {
		t.Fatalf("ran changed alias: %s", worker.jobs[0].Dir)
	}
	// Replacing the stored canonical prefix must fail instead of escaping.
	env = runMessage()
	env.Run = &envelope.Run{Dir: root + "/next"}
	if _, err := mb.Send(env); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, root+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	c := executeOne(t, mb)
	var state string
	if err := mb.db.QueryRow(`SELECT state FROM runs WHERE id=?`, c.runID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || len(worker.jobs) != 1 {
		t.Fatalf("state=%s jobs=%d", state, len(worker.jobs))
	}
}

type desktopAgent struct {
	run  func(context.Context, agent.Job, agent.Report) agent.Outcome
	open func(string) error
}

func (d desktopAgent) Run(ctx context.Context, j agent.Job, r agent.Report) agent.Outcome {
	return d.run(ctx, j, r)
}
func (d desktopAgent) OpenDesktop(id string) error { return d.open(id) }

func TestDesktopOwnershipAndOpening(t *testing.T) {
	for _, state := range []envelope.ResultState{envelope.Completed, envelope.Failed} {
		t.Run(string(state), func(t *testing.T) {
			opened := make(chan string, 1)
			release := make(chan struct{})
			defer close(release)
			var mb *Mailbox
			worker := desktopAgent{
				run: func(_ context.Context, job agent.Job, r agent.Report) agent.Outcome {
					for _, kv := range job.Env {
						if strings.HasPrefix(kv, "BAF_AGENT=") || strings.HasPrefix(kv, "BAF_THREAD=") {
							t.Errorf("identity leaked: %s", kv)
						}
					}
					if job.BafBinary != "/test/baf" || job.BafHome != "/test/home" {
						t.Errorf("prompt config lost: %+v", job)
					}
					var desktop int
					var dir string
					if err := mb.db.QueryRow(`SELECT desktop,dir FROM runs ORDER BY id DESC LIMIT 1`).Scan(&desktop, &dir); err != nil || desktop != 1 || dir != job.Dir {
						t.Errorf("run not recorded before process: desktop=%d dir=%s err=%v", desktop, dir, err)
					}
					r.Thread("private-thread")
					follow := runMessage()
					follow.ToThread = "private-thread"
					_, err := mb.Send(follow)
					wantDoorRejection(t, err, "leave out to_thread")
					return agent.Outcome{State: state, Text: "done"} // thread is known only through Report
				},
				open: func(id string) error {
					opened <- id
					<-release
					return nil
				},
			}
			mb = runMailbox(t, worker, nil)
			env := runMessage()
			env.Run = &envelope.Run{Desktop: true}
			// desktop alone needs no dirs grant.
			peer := tailnet.Peer{Node: "grokbot", Grants: []tailnet.Grant{{To: []string{"worker"}}}}
			if _, err := mb.AcceptPush(context.Background(), env, peer); err != nil {
				t.Fatal(err)
			}
			executeOne(t, mb) // the opener blocks, but recording and execution must return
			select {
			case id := <-opened:
				if id != "private-thread" {
					t.Fatal(id)
				}
			case <-time.After(time.Second):
				t.Fatal("opener not called")
			}
			p, err := mb.Progress("grokbot", env.ID)
			if err != nil || p.State != "done" || p.Run.State != string(state) || p.Run.ThreadID != "" {
				t.Fatalf("progress=%+v err=%v", p, err)
			}
			follow := runMessage()
			follow.ToThread = "private-thread"
			_, err = mb.Send(follow)
			wantDoorRejection(t, err, "handed to the Claude Desktop app")
			if receipt, err := mb.AcceptPush(context.Background(), env, peer); err != nil || !receipt.Duplicate {
				t.Fatalf("retry=%+v err=%v", receipt, err)
			}
			inbox, err := mb.Inbox("me", false, 10)
			if err != nil || len(inbox) != 0 {
				t.Fatalf("automatic reply: %v %v", inbox, err)
			}
		})
	}
}

func TestRunStoreRejections(t *testing.T) {
	mb, _, _ := doorMailbox(t, true)
	for _, to := range []string{"box/codex", "box/me", "openai/dot"} {
		env := runMessage()
		env.To = to
		env.Run = &envelope.Run{Desktop: true}
		_, err := mb.Send(env)
		wantDoorRejection(t, err, "only Claude agents")
	}
	for _, run := range []*envelope.Run{{Dir: "repo"}, {Desktop: true}} {
		env := runMessage()
		env.To = "box/codex"
		env.Run = run
		_, err := mb.FromDoor("openai/dot", env)
		wantDoorRejection(t, err, "Door agents")
		env.Origin = "openai/dot"
		_, err = mb.AcceptPush(context.Background(), env, tailnet.Peer{Node: "relay"})
		wantDoorRejection(t, err, "origin")
		_, err = mb.AcceptForwarded(env, Principal{Node: "openai", Agent: "dot"}, "relay")
		wantDoorRejection(t, err, "origin")
	}
	for _, tc := range []struct {
		kind         envelope.Kind
		thread, want string
	}{{envelope.Request, "", "notice"}, {envelope.Notice, "old", "to_thread"}} {
		env := runMessage()
		env.To = "box/codex"
		env.Kind = tc.kind
		env.ToThread = tc.thread
		env.Run = &envelope.Run{Desktop: true}
		_, err := mb.Send(env)
		wantDoorRejection(t, err, tc.want)
	}
}

type logChannel chan string

func (c logChannel) Write(p []byte) (int, error) {
	c <- string(p)
	return len(p), nil
}

func TestDesktopOpenerErrorOnlyWarns(t *testing.T) {
	logs := make(logChannel, 10)
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	defer slog.SetDefault(previous)
	worker := desktopAgent{
		run: func(context.Context, agent.Job, agent.Report) agent.Outcome {
			return agent.Outcome{State: envelope.Completed, ThreadID: "outcome-thread"}
		},
		open: func(id string) error {
			if id != "outcome-thread" {
				return errors.New("wrong thread")
			}
			return errors.New("test opener failed")
		},
	}
	mb := runMailbox(t, worker, nil)
	env := runMessage()
	env.Run = &envelope.Run{Desktop: true}
	if _, err := mb.Send(env); err != nil {
		t.Fatal(err)
	}
	c := executeOne(t, mb)
	deadline := time.After(time.Second)
	for {
		select {
		case line := <-logs:
			if !strings.Contains(line, "open Claude Desktop") {
				continue
			}
			if !strings.Contains(line, "level=WARN") || !strings.Contains(line, "test opener failed") {
				t.Fatal(line)
			}
			var state string
			if err := mb.db.QueryRow(`SELECT state FROM runs WHERE id=?`, c.runID).Scan(&state); err != nil || state != "completed" {
				t.Fatalf("state=%s error=%v", state, err)
			}
			return
		case <-deadline:
			t.Fatal("opener failure was not logged")
		}
	}
}
