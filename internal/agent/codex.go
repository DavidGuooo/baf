package agent

import (
	"context"
	"encoding/json"

	"github.com/DavidGuooo/baf/internal/envelope"
)

// Codex runs the Codex CLI headless: `codex exec --json` for a new thread,
// `codex exec resume --json ID` to continue one. Facts it relies on were
// checked in T0 (design notes section 8).
type Codex struct {
	Bin     string // path to codex; defaults to "codex"
	Dir     string // working directory for new threads
	Profile string // optional Codex profile (-p) with only what remote requests need
}

// codexEvent is the part of a `codex exec --json` line we read.
type codexEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"` // thread.started
	Message  string `json:"message"`   // error
	Item     struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"` // item.completed
	Error struct {
		Message string `json:"message"`
	} `json:"error"` // turn.failed
}

func (c Codex) Run(ctx context.Context, job Job, onThread func(string)) Outcome {
	bin := c.Bin
	if bin == "" {
		bin = "codex"
	}
	var args []string
	if job.ResumeThread == "" {
		// v1 runs foreign requests in Codex's own auto mode (design notes
		// section 9, Agent layer): it never stops to ask, because nobody is
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
		onThread(job.ResumeThread)
	}
	args = append(args, Prompt(job))

	var (
		threadID  = job.ResumeThread
		lastText  string
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
				onThread(threadID)
			}
		case "item.completed":
			if ev.Item.Type == "agent_message" {
				lastText = ev.Item.Text
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
