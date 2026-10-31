package agent

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/google/uuid"
)

// Claude runs Claude Code headless: `claude -p --output-format stream-json`.
// For a new thread we choose the session id ourselves, so it is recorded
// before the process even starts.
type Claude struct {
	Bin string // path to claude; defaults to "claude"
	Dir string // working directory
}

// claudeLine is the part of every stream-json line we read first.
type claudeLine struct {
	Type string `json:"type"` // system | assistant | user | result | ...
}

// claudeAssistant is an "assistant" line: one message, which may start tools.
type claudeAssistant struct {
	Message struct {
		Content []struct {
			Type  string          `json:"type"` // text | thinking | tool_use
			Name  string          `json:"name"` // tool_use: Bash, Edit, Read, ...
			Input claudeToolInput `json:"input"`
		} `json:"content"`
	} `json:"message"`
}

// claudeToolInput is the part of a tool call's input a Doing line uses.
type claudeToolInput struct {
	Command     string `json:"command"`     // Bash
	FilePath    string `json:"file_path"`   // Read, Edit, Write (absolute)
	Pattern     string `json:"pattern"`     // Grep, Glob
	Query       string `json:"query"`       // WebSearch
	URL         string `json:"url"`         // WebFetch
	Description string `json:"description"` // Task, Agent
}

// claudeResult is the last line, the same object `--output-format json` prints.
type claudeResult struct {
	Result    string `json:"result"`
	IsError   bool   `json:"is_error"`
	SessionID string `json:"session_id"`
	Subtype   string `json:"subtype"`
}

func (c Claude) Run(ctx context.Context, job Job, report Report) Outcome {
	bin := c.Bin
	if bin == "" {
		bin = "claude"
	}
	threadID := job.ResumeThread
	// stream-json prints each step as it happens; with -p, Claude Code
	// refuses it without --verbose.
	args := []string{"-p", "--output-format", "stream-json", "--verbose",
		// Claude Code's own auto mode; anything that would still ask for
		// permission is denied, since nobody is there to answer.
		"--permission-mode", "auto", "--permission-prompts", "none"}
	if threadID == "" {
		threadID = uuid.NewString()
		args = append(args, "--session-id", threadID)
	} else {
		args = append(args, "--resume", threadID)
	}
	report.thread(threadID)
	args = append(args, Prompt(job))

	var (
		out       claudeResult
		gotResult bool
		stdout    = &tailBuffer{max: 4 << 10} // the end of its output, for a failure message
	)
	env := append(job.Env, "BAF_THREAD="+threadID) // so the agent's own `baf send` names its thread
	res := runProcess(ctx, bin, args, c.Dir, env, func(line []byte) {
		stdout.Write(line)
		stdout.Write([]byte{'\n'})
		var l claudeLine
		if json.Unmarshal(line, &l) != nil {
			return
		}
		switch l.Type {
		case "assistant":
			var a claudeAssistant
			if json.Unmarshal(line, &a) != nil {
				return
			}
			for _, part := range a.Message.Content {
				if part.Type == "tool_use" {
					report.doing(claudeDoing(part.Name, part.Input, c.Dir))
				}
			}
		case "result":
			gotResult = json.Unmarshal(line, &out) == nil
		}
	})

	if res.exitCode == 0 && gotResult && !out.IsError {
		return Outcome{State: envelope.Completed, Text: out.Result, ThreadID: threadID}
	}
	detail := out.Subtype
	if !gotResult && len(stdout.buf) > 0 {
		detail = "unexpected output: " + lastLines(strings.TrimSpace(stdout.String()), 10)
	}
	return Outcome{
		State:    envelope.Failed,
		Text:     failureText("Claude Code", res, detail, out.Result),
		ThreadID: threadID,
	}
}

// claudeDoing describes a tool call Claude Code started.
func claudeDoing(tool string, in claudeToolInput, dir string) string {
	switch tool {
	case "Bash":
		return "running: " + in.Command
	case "Edit", "MultiEdit", "Write":
		return "editing " + shortPath(dir, in.FilePath)
	case "Read":
		return "reading " + shortPath(dir, in.FilePath)
	case "Grep", "Glob":
		return "searching: " + in.Pattern
	case "WebSearch":
		return "searching the web: " + in.Query
	case "WebFetch":
		return "fetching " + in.URL
	case "Task", "Agent":
		return "running a subagent: " + in.Description
	}
	return "using " + tool
}
