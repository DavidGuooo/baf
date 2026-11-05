package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/DavidGuooo/baf/internal/envelope"
)

func TestRunFolderOverridesAdapters(t *testing.T) {
	for _, kind := range []string{"claude", "codex"} {
		for _, resume := range []string{"", "old"} {
			t.Run(kind+resume, func(t *testing.T) {
				events := codexOK
				if kind == "claude" {
					events = `echo '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"'"$PWD"'/file.go"}}]}}'
echo '{"type":"result","result":"ok"}'`
				}
				bin, dir := fakeCLI(t, events)
				work := filepath.Join(dir, "override")
				if err := os.Mkdir(work, 0755); err != nil {
					t.Fatal(err)
				}
				// Record cwd at a fixed location, independent of the working directory.
				script := readFile(t, bin)
				script = strings.Replace(script, "#!/bin/sh\n", "#!/bin/sh\npwd > '"+dir+"/cwd'\n", 1)
				if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				work, err := filepath.EvalSymlinks(work)
				if err != nil {
					t.Fatal(err)
				}
				job := testJob(resume)
				job.Dir = work
				var adapter Adapter = Codex{Bin: bin, Dir: dir}
				if kind == "claude" {
					adapter = Claude{Bin: bin, Dir: dir}
				}
				var doing string
				out := adapter.Run(context.Background(), job, Report{Doing: func(s string) { doing = s }})
				if out.State != envelope.Completed {
					t.Fatalf("outcome=%+v", out)
				}
				got := strings.TrimSpace(readFile(t, dir+"/cwd"))
				want, err := filepath.EvalSymlinks(work)
				if err != nil || got != want {
					t.Fatalf("cwd=%q want %q error=%v", got, want, err)
				}
				if kind == "claude" && doing != "reading file.go" {
					t.Fatalf("doing=%q", doing)
				}
				if kind == "codex" && resume == "" && !strings.Contains(readFile(t, dir+"/args"), "-C\n"+work+"\n") {
					t.Fatal("missing -C override")
				}
			})
		}
	}
}

func TestDesktopEnvironmentAndPrompt(t *testing.T) {
	t.Setenv("BAF_AGENT", "inherited")
	t.Setenv("BAF_THREAD", "secret-inherited")
	bin, dir := fakeCLI(t, `echo '{"type":"result","result":"ok"}'`)
	job := testJob("")
	job.Message.Kind = envelope.Notice
	job.Message.Run = &envelope.Run{Desktop: true}
	job.BafBinary = "/Applications/My Tools/baf"
	job.BafHome = "/tmp/owner's baf"
	job.Env = []string{"BAF_AGENT=claude", "BAF_THREAD=secret-extra", "BAF_HOME=/tmp/fake-home", "PATH=" + os.Getenv("PATH")}
	out := (Claude{Bin: bin, Dir: dir}).Run(context.Background(), job, Report{})
	if out.State != envelope.Completed || out.ThreadID == "" {
		t.Fatalf("outcome=%+v", out)
	}
	env := readFile(t, dir+"/env")
	for _, bad := range []string{"BAF_AGENT=", "BAF_THREAD="} {
		if strings.Contains(env, bad) {
			t.Fatalf("leaked %s", bad)
		}
	}
	for _, want := range []string{"BAF_HOME=/tmp/fake-home", "PATH="} {
		if !strings.Contains(env, want) {
			t.Fatalf("missing %s", want)
		}
	}
	prompt := Prompt(job)
	command := `BAF_HOME='/tmp/owner'\''s baf' '/Applications/My Tools/baf' send box/me --as me --to-thread mine "<outcome>"`
	for _, want := range []string{command, "After this run, baf opens this session in the Claude Desktop app for the owner", "don't message the sender during this run", "When the owner says the work is done"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("missing %q in %s", want, prompt)
		}
	}
	if strings.Contains(prompt, "To message them yourself") || strings.Contains(prompt, out.ThreadID) {
		t.Fatalf("wrong prompt: %s", prompt)
	}
	job.Message.FromThread = ""
	job.BafHome = ""
	prompt = Prompt(job)
	if strings.Contains(prompt, "--to-thread") || strings.Contains(prompt, "BAF_HOME=") {
		t.Fatalf("unexpected optional fields: %s", prompt)
	}
	if !strings.Contains(prompt, `'/Applications/My Tools/baf' send box/me --as me "<outcome>"`) {
		t.Fatal(prompt)
	}
}

func TestOpenDesktopUsesInjectedCommand(t *testing.T) {
	called := false
	failure := errors.New("app unavailable")
	c := Claude{OpenCommand: func(_ context.Context, name string, args ...string) error {
		called = true
		if name != "open" || len(args) != 1 || args[0] != "claude://resume?session=thread-id" {
			t.Fatalf("command=%s %v", name, args)
		}
		return failure
	}}
	err := c.OpenDesktop("thread-id")
	if runtime.GOOS == "darwin" {
		if !called || !errors.Is(err, failure) {
			t.Fatalf("called=%v error=%v", called, err)
		}
	} else if called || err == nil {
		t.Fatalf("called=%v error=%v", called, err)
	}
}
