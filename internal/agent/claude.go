package agent

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/google/uuid"
)

// Claude runs Claude Code headless: `claude -p --output-format json`. For a
// new thread we choose the session id ourselves, so it is recorded before
// the process even starts.
type Claude struct {
	Bin string // path to claude; defaults to "claude"
	Dir string // working directory
}

// claudeOutput is the single JSON object `claude -p --output-format json` prints.
type claudeOutput struct {
	Result    string `json:"result"`
	IsError   bool   `json:"is_error"`
	SessionID string `json:"session_id"`
	Subtype   string `json:"subtype"`
}

func (c Claude) Run(ctx context.Context, job Job, onThread func(string)) Outcome {
	bin := c.Bin
	if bin == "" {
		bin = "claude"
	}
	threadID := job.ResumeThread
	args := []string{"-p", "--output-format", "json",
		// Claude Code's own auto mode; anything that would still ask for
		// permission is denied, since nobody is there to answer.
		"--permission-mode", "auto", "--permission-prompts", "none"}
	if threadID == "" {
		threadID = uuid.NewString()
		args = append(args, "--session-id", threadID)
	} else {
		args = append(args, "--resume", threadID)
	}
	onThread(threadID)
	args = append(args, Prompt(job))

	var stdout strings.Builder
	env := append(job.Env, "BAF_THREAD="+threadID) // so the agent's own `baf send` names its thread
	res := runProcess(ctx, bin, args, c.Dir, env, func(line []byte) {
		stdout.Write(line)
		stdout.WriteByte('\n')
	})

	var out claudeOutput
	parseErr := json.Unmarshal([]byte(stdout.String()), &out)
	if res.exitCode == 0 && parseErr == nil && !out.IsError {
		return Outcome{State: envelope.Completed, Text: out.Result, ThreadID: threadID}
	}
	detail := out.Subtype
	if parseErr != nil && stdout.Len() > 0 {
		detail = "unexpected output: " + lastLines(strings.TrimSpace(stdout.String()), 10)
	}
	return Outcome{
		State:    envelope.Failed,
		Text:     failureText("Claude Code", res, detail, out.Result),
		ThreadID: threadID,
	}
}
