// Package agent is the seam between a mailbox and the agents it serves.
// Each kind of agent gets an Adapter that knows how
// to start it, how to continue one of its threads, and how its run ended.
package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/DavidGuooo/baf/internal/envelope"
)

// Job is one message handed to an agent.
type Job struct {
	Message envelope.Envelope
	Sender  envelope.Address // who the mailbox believes sent it

	// ResumeThread is the agent's own thread to continue. Empty means start
	// a new thread. The mailbox has already checked the sender may use it.
	ResumeThread string

	// Env is extra environment for the agent's process (BAF_AGENT and so on),
	// so the agent can call `baf send` itself.
	Env []string

	// Files are the message's files, already on this machine (ADR-0007).
	Files []File
}

// File is one of a message's files as the agent gets it: a path it can read.
type File struct {
	Name string
	Size int64
	Path string
}

// Outcome is how a run ended.
type Outcome struct {
	State    envelope.ResultState // Completed or Failed
	Text     string               // the agent's final message, or what went wrong
	ThreadID string               // the agent's thread, if it got that far
}

// Adapter runs one kind of agent.
//
// Run blocks until the agent's run ends, telling report what it learns on
// the way. When ctx is cancelled (baf is stopping), Run stops the agent's
// process and returns a Failed outcome.
type Adapter interface {
	Run(ctx context.Context, job Job, report Report) Outcome
}

// Report is how an adapter tells the mailbox about a run while it runs.
// Either func may be nil.
type Report struct {
	// Thread is called as soon as the agent's thread id is known, before the
	// agent does any work if the agent allows it, so the mailbox can record
	// the id before anything can go wrong.
	Thread func(threadID string)

	// Doing is called with one line saying what the agent has just started
	// doing ("running: go test ./..."). Adapters read it from the agent's own
	// output, so the agent never has to report anything (ADR-0006).
	Doing func(text string)
}

func (r Report) thread(id string) {
	if r.Thread != nil {
		r.Thread(id)
	}
}

func (r Report) doing(text string) {
	if r.Doing != nil && text != "" {
		r.Doing(oneLine(text, maxDoing))
	}
}

// Func turns a plain function into an Adapter. Tests use it as the fake agent.
type Func func(ctx context.Context, job Job, report Report) Outcome

func (f Func) Run(ctx context.Context, job Job, report Report) Outcome {
	return f(ctx, job, report)
}

// maxDoing is the longest a Doing line gets.
const maxDoing = 160

// oneLine squeezes s onto one line of at most n characters.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// shortPath shows p relative to the agent's working directory when it is
// inside it, so a Doing line names files the way the project does.
func shortPath(dir, p string) string {
	rel, err := filepath.Rel(dir, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return p
	}
	return rel
}

// Prompt is the text an agent receives for a message: who sent it, what kind
// it is, what happens to the agent's answer, and then the message itself.
func Prompt(job Job) string {
	m := job.Message
	var b strings.Builder
	fmt.Fprintf(&b, "[baf] You received a %s from %s (message id %s).\n", m.Kind, job.Sender, m.ID)
	switch m.Kind {
	case envelope.Request:
		b.WriteString("Your final message will be sent back to them automatically as the result.\n")
	case envelope.Result:
		fmt.Fprintf(&b, "It answers your request %s. Outcome: %s.\n", m.ReplyTo, m.Body.State)
		b.WriteString("Nothing is sent back automatically.\n")
	case envelope.Notice:
		b.WriteString("No reply is expected and nothing is sent back automatically.\n")
	}
	if m.FromThread != "" {
		fmt.Fprintf(&b, "To message them yourself: baf send %s --to-thread %s \"text\"\n", job.Sender, m.FromThread)
	} else {
		fmt.Fprintf(&b, "To message them yourself: baf send %s \"text\"\n", job.Sender)
	}
	if len(job.Files) > 0 {
		b.WriteString("Files sent with it (saved on this machine):\n")
		for _, f := range job.Files {
			fmt.Fprintf(&b, "- %s (%d bytes): %s\n", f.Name, f.Size, f.Path)
		}
	}
	b.WriteString("--- message ---\n")
	b.WriteString(m.Body.Text)
	return b.String()
}
