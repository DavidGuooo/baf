// Package envelope defines the message that moves between mailboxes and
// the node/agent address format.
package envelope

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// Version is the envelope format this build speaks.
const Version = 1

// Kind says what a message expects. A receipt is not a message (ADR-0004),
// so it has no kind here.
type Kind string

const (
	Notice  Kind = "notice"  // expects no answer
	Request Kind = "request" // the receiving mailbox answers it with a Result
	Result  Kind = "result"  // answers a Request; never answered automatically
)

// ResultState is how a Request ended, carried in a Result's body.
type ResultState string

const (
	Completed ResultState = "completed" // the run finished normally
	Failed    ResultState = "failed"    // we know the run did not finish
	Rejected  ResultState = "rejected"  // the request was refused before any run
	Unknown   ResultState = "unknown"   // we lost track; it may have finished
)

// Envelope is one immutable message. Its ID is chosen by the sender and
// stays the same across retries, so the receiver can drop duplicates.
type Envelope struct {
	V    int    `json:"v"`
	ID   string `json:"id"`
	Kind Kind   `json:"kind"`
	To   string `json:"to"` // "node/agent"

	// FromAgent is the sending agent's name on its node. The node itself
	// never comes from the envelope: the receiver asks Tailscale.
	FromAgent string `json:"from_agent"`

	// Origin is filled in only by the relay, naming the original sender.
	// It is ignored unless the connecting machine holds relay: true.
	Origin string `json:"origin,omitempty"`

	// FromThread is the sender's own thread; ToThread is the receiver's
	// thread to continue (empty = start a new one).
	FromThread string `json:"from_thread,omitempty"`
	ToThread   string `json:"to_thread,omitempty"`

	ReplyTo string `json:"reply_to,omitempty"` // set on a Result
	Wake    string `json:"wake,omitempty"`     // a hint only; unused for now
	Body    Body   `json:"body"`
}

// Body is the message content. Files are not supported yet.
type Body struct {
	Text  string      `json:"text"`
	State ResultState `json:"state,omitempty"` // only on a Result
}

// NewID returns a new ULID: unique, and sortable by creation time.
func NewID() string {
	return ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader).String()
}

// Validate checks the fields every message must have.
func (e *Envelope) Validate() error {
	if e.V != Version {
		return fmt.Errorf("unsupported envelope version %d", e.V)
	}
	if e.ID == "" {
		return errors.New("missing id")
	}
	switch e.Kind {
	case Notice, Request:
	case Result:
		if e.ReplyTo == "" {
			return errors.New("a result must name the request it answers (reply_to)")
		}
		switch e.Body.State {
		case Completed, Failed, Rejected, Unknown:
		default:
			return fmt.Errorf("unknown result state %q", e.Body.State)
		}
	default:
		return fmt.Errorf("unknown kind %q", e.Kind)
	}
	if _, err := ParseAddress(e.To); err != nil {
		return fmt.Errorf("bad to: %w", err)
	}
	if err := checkName(e.FromAgent); err != nil {
		return fmt.Errorf("bad from_agent: %w", err)
	}
	return nil
}

// Address is "node/agent". Tailscale vouches for the node; the agent part
// is the node's claim.
type Address struct {
	Node  string
	Agent string
}

func (a Address) String() string { return a.Node + "/" + a.Agent }

// ParseAddress reads "node/agent".
func ParseAddress(s string) (Address, error) {
	node, agent, ok := strings.Cut(s, "/")
	if !ok {
		return Address{}, fmt.Errorf("%q is not node/agent", s)
	}
	if err := checkName(node); err != nil {
		return Address{}, fmt.Errorf("node: %w", err)
	}
	if err := checkName(agent); err != nil {
		return Address{}, fmt.Errorf("agent: %w", err)
	}
	return Address{Node: node, Agent: agent}, nil
}

// checkName accepts the characters a MagicDNS label or agent name may use.
func checkName(s string) error {
	if s == "" {
		return errors.New("empty name")
	}
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_'
		if !ok {
			return fmt.Errorf("%q: use lowercase letters, digits, - and _", s)
		}
	}
	return nil
}
