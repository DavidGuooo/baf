package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DavidGuooo/baf/internal/envelope"
)

// fakeCLI writes a shell script standing in for codex or claude. The script
// saves its arguments and environment to files, then runs body.
func fakeCLI(t *testing.T, body string) (bin, dir string) {
	t.Helper()
	dir = t.TempDir()
	bin = filepath.Join(dir, "fake")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + dir + "/args\"\nenv > \"" + dir + "/env\"\n" + body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func testJob(resume string) Job {
	return Job{
		Message: envelope.Envelope{V: 1, ID: "01TEST", Kind: envelope.Request, To: "box/codex",
			FromAgent: "me", FromThread: "mine", Body: envelope.Body{Text: "say ok"}},
		Sender:       envelope.Address{Node: "box", Agent: "me"},
		ResumeThread: resume,
		Env:          []string{"BAF_AGENT=codex"},
	}
}

// codexOK prints the events a real `codex exec --json` printed in T0.
const codexOK = `cat <<'EOF'
{"type":"thread.started","thread_id":"019a-thread"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"i0","type":"reasoning","text":"thinking"}}
{"type":"item.completed","item":{"id":"i1","type":"agent_message","text":"ok"}}
{"type":"turn.completed","usage":{"input_tokens":1}}
EOF`

func TestCodexNewThread(t *testing.T) {
	bin, dir := fakeCLI(t, codexOK)
	var reported []string
	out := Codex{Bin: bin, Dir: dir}.Run(context.Background(), testJob(""), func(id string) { reported = append(reported, id) })

	if out.State != envelope.Completed || out.Text != "ok" || out.ThreadID != "019a-thread" {
		t.Fatalf("outcome = %+v", out)
	}
	if len(reported) != 1 || reported[0] != "019a-thread" {
		t.Fatalf("onThread got %v", reported)
	}
	args := readFile(t, dir+"/args")
	for _, want := range []string{"exec\n--json\n--skip-git-repo-check\n-C\n" + dir + "\n--approve-for-me\n", "say ok"} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q:\n%s", want, args)
		}
	}
	if !strings.Contains(readFile(t, dir+"/env"), "BAF_AGENT=codex") {
		t.Error("BAF_AGENT not passed to the run")
	}
}

func TestCodexResume(t *testing.T) {
	bin, dir := fakeCLI(t, strings.Replace(codexOK, "019a-thread", "old-thread", 1))
	var reported []string
	out := Codex{Bin: bin, Dir: dir}.Run(context.Background(), testJob("old-thread"), func(id string) { reported = append(reported, id) })
	if out.State != envelope.Completed || out.ThreadID != "old-thread" {
		t.Fatalf("outcome = %+v", out)
	}
	if len(reported) != 1 {
		t.Fatalf("onThread got %v, want the resumed id once", reported)
	}
	args := readFile(t, dir+"/args")
	if !strings.Contains(args, "exec\nresume\n--json\n--skip-git-repo-check\n-c\nsandbox_mode=\"workspace-write\"\nold-thread\n") {
		t.Fatalf("args:\n%s", args)
	}
	if !strings.Contains(readFile(t, dir+"/env"), "BAF_THREAD=old-thread") {
		t.Error("BAF_THREAD not passed to a resumed run")
	}
}

func TestCodexFailsWithoutTurnCompleted(t *testing.T) {
	bin, dir := fakeCLI(t, `echo '{"type":"thread.started","thread_id":"t"}'
echo 'no rollout found for thread id' >&2
exit 1`)
	out := Codex{Bin: bin, Dir: dir}.Run(context.Background(), testJob(""), func(string) {})
	if out.State != envelope.Failed {
		t.Fatalf("state = %s", out.State)
	}
	for _, want := range []string{"exited with code 1", "no rollout found"} {
		if !strings.Contains(out.Text, want) {
			t.Errorf("failure text missing %q: %q", want, out.Text)
		}
	}
	if out.ThreadID != "t" {
		t.Errorf("thread id lost: %q", out.ThreadID)
	}
}

func TestCodexStoppedOnCancel(t *testing.T) {
	bin, dir := fakeCLI(t, `echo '{"type":"thread.started","thread_id":"t"}'
sleep 30`)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	start := time.Now()
	out := Codex{Bin: bin, Dir: dir}.Run(ctx, testJob(""), func(string) {})
	if out.State != envelope.Failed {
		t.Fatalf("state = %s", out.State)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("stopping took %s", took)
	}
}

func TestClaudeChoosesThreadIDUpFront(t *testing.T) {
	bin, dir := fakeCLI(t, `echo '{"type":"result","subtype":"success","is_error":false,"result":"hello","session_id":"x"}'`)
	var reported string
	out := Claude{Bin: bin, Dir: dir}.Run(context.Background(), testJob(""), func(id string) { reported = id })
	if out.State != envelope.Completed || out.Text != "hello" {
		t.Fatalf("outcome = %+v", out)
	}
	if len(reported) != 36 || out.ThreadID != reported {
		t.Fatalf("thread id %q, reported %q", out.ThreadID, reported)
	}
	args := readFile(t, dir+"/args")
	if !strings.Contains(args, "--session-id\n"+reported+"\n") || !strings.Contains(args, "--permission-prompts\nnone\n") {
		t.Fatalf("args:\n%s", args)
	}
	if !strings.Contains(readFile(t, dir+"/env"), "BAF_THREAD="+reported) {
		t.Error("BAF_THREAD not passed")
	}
}

func TestClaudeResumeAndError(t *testing.T) {
	bin, dir := fakeCLI(t, `echo '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"","session_id":"s"}'`)
	out := Claude{Bin: bin, Dir: dir}.Run(context.Background(), testJob("s"), func(string) {})
	if out.State != envelope.Failed || !strings.Contains(out.Text, "error_during_execution") {
		t.Fatalf("outcome = %+v", out)
	}
	if !strings.Contains(readFile(t, dir+"/args"), "--resume\ns\n") {
		t.Fatal("did not resume")
	}
}

func TestPromptSaysWhatHappensToTheAnswer(t *testing.T) {
	p := Prompt(testJob(""))
	for _, want := range []string{"request from box/me", "sent back to them automatically", "--to-thread mine", "say ok"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
}

func TestCleanEnvDropsNestedClaudeMarkers(t *testing.T) {
	got := cleanEnv([]string{"CLAUDECODE=1", "PATH=/bin", "CLAUDE_CODE_ENTRYPOINT=cli"})
	if len(got) != 1 || got[0] != "PATH=/bin" {
		t.Fatalf("got %v", got)
	}
}
