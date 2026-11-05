package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/DavidGuooo/baf/internal/envelope"
)

// Codex runs the Codex CLI headless: `codex exec --json` for a new thread,
// `codex exec resume --json ID` to continue one. Facts it relies on were
// checked on a real install (T0).
type Codex struct {
	Bin     string // path to codex; defaults to "codex"
	Dir     string // working directory for new threads
	Profile string // optional Codex profile (-p) with only what remote requests need
}

// codexEvent is the part of a `codex exec --json` line we read.
type codexEvent struct {
	Type     string    `json:"type"`
	ThreadID string    `json:"thread_id"` // thread.started
	Message  string    `json:"message"`   // error
	Item     codexItem `json:"item"`      // item.started, item.completed
	Error    struct {
		Message string `json:"message"`
	} `json:"error"` // turn.failed
}

// codexItem is one step of a Codex turn. Field names were checked on real
// runs (agent_message, command_execution, file_change); mcp_tool_call and
// web_search follow Codex's documented event format.
type codexItem struct {
	Type    string `json:"type"`
	Text    string `json:"text"`    // agent_message
	Command string `json:"command"` // command_execution: "/bin/zsh -lc 'go test ./...'"
	Changes []struct {
		Path string `json:"path"` // absolute
	} `json:"changes"` // file_change
	Server string `json:"server"` // mcp_tool_call
	Tool   string `json:"tool"`
	Query  string `json:"query"` // web_search
}

func (c Codex) Run(ctx context.Context, job Job, report Report) Outcome {
	if job.Dir != "" {
		c.Dir = job.Dir
	}
	bin := c.Bin
	if bin == "" {
		bin = "codex"
	}
	var args []string
	if job.ResumeThread == "" {
		// v1 runs foreign requests in Codex's own auto mode: it never stops to ask, because nobody is
		// there to answer. --approve-for-me implies the workspace-write
		// sandbox, and Codex refuses -s alongside it.
		args = []string{"exec", "--json", "--skip-git-repo-check", "-C", c.Dir, "--approve-for-me"}
		if c.Profile != "" {
			args = append(args, "-p", c.Profile)
		}
	} else {
		// `resume` takes neither -s nor --approve-for-me. Pin the sandbox so a
		// resumed thread can never run with more access than a new one.
		args = []string{"exec", "resume", "--json", "--skip-git-repo-check",
			"-c", `sandbox_mode="workspace-write"`, job.ResumeThread}
		report.thread(job.ResumeThread)
	}
	args = append(args, Prompt(job))

	var (
		threadID  = job.ResumeThread
		lastText  string
		lastDoing string
		errText   string
		completed bool
	)
	env := job.Env
	if threadID != "" {
		// Known only when resuming; a new thread's id arrives after the start.
		env = append(env, "BAF_THREAD="+threadID)
	}
	res := runProcess(ctx, bin, args, c.Dir, env, func(line []byte) {
		var ev codexEvent
		if json.Unmarshal(line, &ev) != nil {
			return // not an event line
		}
		switch ev.Type {
		case "thread.started":
			// The first event, before any work: record the id at once.
			if ev.ThreadID != "" && ev.ThreadID != threadID {
				threadID = ev.ThreadID
				report.thread(threadID)
			}
		case "item.started", "item.completed":
			if ev.Type == "item.completed" && ev.Item.Type == "agent_message" {
				lastText = ev.Item.Text
			}
			// Most steps arrive twice, started and completed: say each once.
			if d := codexDoing(ev.Item, c.Dir); d != "" && d != lastDoing {
				lastDoing = d
				report.doing(d)
			}
		case "turn.completed":
			completed = true
		case "turn.failed":
			errText = ev.Error.Message
		case "error":
			errText = ev.Message
		}
	})

	if completed && res.exitCode == 0 {
		return Outcome{State: envelope.Completed, Text: lastText, ThreadID: threadID}
	}
	return Outcome{
		State:    envelope.Failed,
		Text:     failureText("Codex", res, errText, lastText),
		ThreadID: threadID,
	}
}

// codexDoing describes a step Codex took, or returns "" for steps that are
// not actions (its messages and reasoning).
func codexDoing(item codexItem, dir string) string {
	switch item.Type {
	case "command_execution":
		return "running: " + unwrapShell(item.Command)
	case "file_change":
		if len(item.Changes) == 0 {
			return "editing files"
		}
		d := "editing " + shortPath(dir, item.Changes[0].Path)
		if more := len(item.Changes) - 1; more > 0 {
			d += fmt.Sprintf(" and %d more", more)
		}
		return d
	case "mcp_tool_call":
		return "calling " + strings.Trim(item.Server+"."+item.Tool, ".")
	case "web_search":
		return "searching the web: " + item.Query
	}
	return ""
}

// unwrapShell turns Codex's "/bin/zsh -lc 'go test ./...'" into the command
// itself.
func unwrapShell(cmd string) string {
	for _, flag := range []string{" -lc ", " -c "} {
		shell, rest, ok := strings.Cut(cmd, flag)
		if !ok || strings.ContainsAny(shell, " \t") || !strings.HasSuffix(shell, "sh") {
			continue
		}
		if len(rest) >= 2 && rest[0] == '\'' && rest[len(rest)-1] == '\'' {
			rest = strings.ReplaceAll(rest[1:len(rest)-1], `'\''`, `'`)
		}
		return rest
	}
	return cmd
}
