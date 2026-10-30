// Package agent is the seam between a mailbox and the agents it serves
// (design notes section 4). Each kind of agent gets an Adapter that knows how
// to start it, how to continue one of its threads, and how its run ended.
package agent

import (
	"context"
	"fmt"
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
}

// Outcome is how a run ended.
type Outcome struct {
	State    envelope.ResultState // Completed or Failed
	Text     string               // the agent's final message, or what went wrong
	ThreadID string               // the agent's thread, if it got that far
}

// Adapter runs one kind of agent.
//
// Run blocks until the agent's run ends. It calls onThread as soon as the
// agent's thread id is known, before the agent does any work if the agent
// allows it, so the mailbox can record the id before anything can go wrong.
// When ctx is cancelled (baf is stopping), Run stops the agent's process and
// returns a Failed outcome.
type Adapter interface {
	Run(ctx context.Context, job Job, onThread func(threadID string)) Outcome
}

// Func turns a plain function into an Adapter. Tests use it as the fake agent.
type Func func(ctx context.Context, job Job, onThread func(string)) Outcome

func (f Func) Run(ctx context.Context, job Job, onThread func(string)) Outcome {
	return f(ctx, job, onThread)
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
	b.WriteString("--- message ---\n")
	b.WriteString(m.Body.Text)
	return b.String()
}
