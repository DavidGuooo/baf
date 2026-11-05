package agent

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// stopGrace is how long an agent gets to exit after SIGTERM before it is killed.
const stopGrace = 10 * time.Second

// processResult is what runProcess learned about one agent process.
type processResult struct {
	exitCode   int    // -1 if the process never started or was killed by a signal
	stderrTail string // the last few KB of stderr, for failure messages
	startErr   error  // the process could not be started at all
}

// runProcess starts bin with args in dir and calls onLine for each line it
// writes to stdout. Its stdin is empty (Codex would otherwise wait to read
// it). When ctx is cancelled, the whole process group gets SIGTERM, then
// SIGKILL after stopGrace.
func runProcess(ctx context.Context, bin string, args []string, dir string, extraEnv []string,
	onLine func([]byte)) processResult {
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return processResult{exitCode: -1, startErr: err}
		}
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(cleanEnv(WithoutIdentity(os.Environ())), extraEnv...)
	cmd.Stdin = nil // reads from /dev/null
	// Own process group, so stopping the agent also stops the tools it started.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = stopGrace

	stderr := &tailBuffer{max: 8 << 10}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return processResult{exitCode: -1, startErr: err}
	}
	if err := cmd.Start(); err != nil {
		return processResult{exitCode: -1, startErr: err}
	}

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 16<<20) // agent events can be long
	for sc.Scan() {
		onLine(sc.Bytes())
	}
	err = cmd.Wait()

	res := processResult{exitCode: 0, stderrTail: strings.TrimSpace(stderr.String())}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.exitCode = exitErr.ExitCode()
	} else if err != nil {
		res.exitCode = -1
	}
	if ctx.Err() != nil {
		// Make sure nothing in the group outlives a cancelled run.
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return res
}

// cleanEnv drops variables that would make a nested Claude Code think it is
// running inside another Claude Code session.
func cleanEnv(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "CLAUDECODE=") || strings.HasPrefix(kv, "CLAUDE_CODE_ENTRYPOINT=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// WithoutIdentity prevents a run from inheriting the daemon's caller identity.
// Desktop runs also use it on their extra environment to speak as the owner.
func WithoutIdentity(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, "BAF_AGENT=") && !strings.HasPrefix(kv, "BAF_THREAD=") {
			out = append(out, kv)
		}
	}
	return out
}

// tailBuffer keeps only the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }

// failureText builds a readable explanation of a run that did not finish.
func failureText(agentName string, res processResult, detail, lastMessage string) string {
	var b strings.Builder
	switch {
	case res.startErr != nil:
		b.WriteString(agentName + " could not be started: " + res.startErr.Error())
	case res.exitCode == 0:
		b.WriteString(agentName + " exited without finishing its turn")
	default:
		b.WriteString(agentName + " exited with code " + strconv.Itoa(res.exitCode))
	}
	if detail != "" {
		b.WriteString("\nError: " + detail)
	}
	if lastMessage != "" {
		b.WriteString("\nLast message: " + lastMessage)
	}
	if res.stderrTail != "" {
		b.WriteString("\nEnd of its error output:\n" + lastLines(res.stderrTail, 20))
	}
	return b.String()
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
