// Package envelope defines the message that moves between mailboxes and
// the node/agent address format.
package envelope

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

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

	// Origin names the Door agent for whom the Relay forwards.
	// Receivers accept it only under a relay grant naming that Door agent.
	Origin string `json:"origin,omitempty"`

	// FromThread is the sender's own thread; ToThread is the receiver's
	// thread to continue (empty = start a new one).
	FromThread string `json:"from_thread,omitempty"`
	ToThread   string `json:"to_thread,omitempty"`

	ReplyTo string `json:"reply_to,omitempty"` // set on a Result
	Wake    string `json:"wake,omitempty"`     // a hint only; unused for now
	Body    Body   `json:"body"`
}

// Body is the message content.
type Body struct {
	Text  string      `json:"text"`
	Files []File      `json:"files,omitempty"` // their bytes travel ahead of the message (ADR-0007)
	State ResultState `json:"state,omitempty"` // only on a Result
}

// File names one file sent with a message. The envelope carries only its
// name, hash and size; the sending mailbox hands the bytes over first, and
// the receiver checks them against SHA256 (ADR-0007).
type File struct {
	Name   string `json:"name"`   // a plain file name, no directories
	SHA256 string `json:"sha256"` // 64 lowercase hex digits
	Size   int64  `json:"size"`   // bytes
}

// Limits every mailbox applies to files, sending and receiving alike.
const (
	MaxFiles    = 20        // per message
	MaxFileSize = 100 << 20 // bytes, per file
)

// NewID returns a new ULID: unique, and sortable by creation time.
func NewID() string {
	return ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader).String()
}

// Validate checks the fields every message must have.
func (e *Envelope) Validate() error {
	if e.V != Version {
		return fmt.Errorf("unsupported envelope version %d", e.V)
	}
	if err := checkID(e.ID); err != nil {
		return err
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
	return checkFiles(e.Body.Files)
}

func checkFiles(files []File) error {
	if len(files) > MaxFiles {
		return fmt.Errorf("%d files; a message may carry at most %d", len(files), MaxFiles)
	}
	seen := map[string]bool{}
	for _, f := range files {
		if err := CheckFileName(f.Name); err != nil {
			return err
		}
		// Case-insensitive file systems (macOS) would put both in one file.
		key := strings.ToLower(f.Name)
		if seen[key] {
			return fmt.Errorf("two files named %q", f.Name)
		}
		seen[key] = true
		if !isSHA256(f.SHA256) {
			return fmt.Errorf("file %q: sha256 must be 64 lowercase hex digits", f.Name)
		}
		if f.Size < 0 || f.Size > MaxFileSize {
			return fmt.Errorf("file %q is %d bytes; the limit is %d MB", f.Name, f.Size, MaxFileSize>>20)
		}
	}
	return nil
}

// CheckFileName accepts a plain file name that is safe to create inside a
// directory of its own: no directories, no control characters.
func CheckFileName(name string) error {
	switch {
	case name == "" || name == "." || name == "..":
		return fmt.Errorf("bad file name %q", name)
	case len(name) > 255:
		return fmt.Errorf("file name %q… is longer than 255 bytes", name[:40])
	case !utf8.ValidString(name):
		return fmt.Errorf("file name %q is not valid UTF-8", name)
	}
	for _, r := range name {
		if r == '/' || r == '\\' || r < 0x20 || r == 0x7f {
			return fmt.Errorf("file name %q must be a plain name, without / \\ or control characters", name)
		}
	}
	return nil
}

func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// checkID accepts the ids baf makes (ULIDs) and others of the same safe
// shape. An id also names a directory of received files, so it may hold
// only letters, digits, - and _.
func checkID(id string) error {
	if id == "" {
		return errors.New("missing id")
	}
	if len(id) > 64 {
		return fmt.Errorf("id %q… is longer than 64 characters", id[:20])
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return fmt.Errorf("id %q: use letters, digits, - and _", id)
		}
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
