package mailbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/DavidGuooo/baf/internal/envelope"
)

// Progress (GLOSSARY) is what this mailbox can tell the sender of a message
// it stored: whether the agent's run has started, what the agent is doing,
// and how the run ended. The sender's mailbox asks for it; it is never sent
// as a message (ADR-0006).
type Progress struct {
	ID       string    `json:"id"`
	To       string    `json:"to"` // node/agent
	StoredAt time.Time `json:"stored_at"`

	// State is the message's state here: pending (stored, its run not
	// started yet), running, done (its run ended), or inbox (an agent baf
	// doesn't run, which reads its own mail).
	State  string     `json:"state"`
	ReadAt *time.Time `json:"read_at,omitempty"` // first opened: by its run, or with `baf read`

	Run *RunProgress `json:"run,omitempty"` // once a run has started
}

// RunProgress is the run that handles a message.
type RunProgress struct {
	State     string     `json:"state"` // running | completed | failed | unknown
	ThreadID  string     `json:"thread_id,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	Doing     string     `json:"doing,omitempty"` // what the agent last started doing
	DoingAt   *time.Time `json:"doing_at,omitempty"`
}

// Progress reports on message id from senderNode. A message from any other
// node is ErrNotFound, so a node sees only its own messages.
func (m *Mailbox) Progress(senderNode, id string) (Progress, error) {
	var (
		p                       Progress
		toAgent                 string
		received                int64
		readAt                  sql.NullInt64
		runState, thread, doing sql.NullString
		started, ended, doingAt sql.NullInt64
	)
	err := m.db.QueryRow(`SELECT m.to_agent, m.state, m.received_at, m.read_at,
			r.state, r.thread_id, r.started_at, r.ended_at, r.doing, r.doing_at
		FROM messages m LEFT JOIN runs r ON r.message_seq = m.seq
		WHERE m.sender_node = ? AND m.id = ?
		ORDER BY r.id DESC LIMIT 1`, senderNode, id).Scan(
		&toAgent, &p.State, &received, &readAt, &runState, &thread, &started, &ended, &doing, &doingAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Progress{}, ErrNotFound
	}
	if err != nil {
		return Progress{}, err
	}
	p.ID, p.To = id, Principal{Node: m.node, Agent: toAgent}.String()
	p.StoredAt, p.ReadAt = time.UnixMilli(received), timePtr(readAt)
	if runState.Valid {
		p.Run = &RunProgress{
			State:     runState.String,
			ThreadID:  thread.String,
			StartedAt: time.UnixMilli(started.Int64),
			EndedAt:   timePtr(ended),
			Doing:     doing.String,
			DoingAt:   timePtr(doingAt),
		}
	}
	return p, nil
}

// SentStatus is what this node knows, or can find out, about a message one
// of its agents sent, for `baf status`. Each part is a separate observation,
// and a part nobody observed is left out.
type SentStatus struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	FromAgent string    `json:"from_agent"`
	To        string    `json:"to"`
	SentAt    time.Time `json:"sent_at"`

	// Handover is the message's outbox entry, for a message to another node.
	Handover *OutItem `json:"handover,omitempty"`

	// Receiver is the receiving mailbox's Progress. Another node is asked
	// only once it has the message and while no Result has come back;
	// ReceiverError says why asking failed.
	Receiver      *Progress `json:"receiver,omitempty"`
	ReceiverError string    `json:"receiver_error,omitempty"`

	// Result is the Result that answered a request, once it arrived here.
	Result *ResultInfo `json:"result,omitempty"`
}

// ResultInfo names the Result that answered a request; `baf read ID` shows it.
type ResultInfo struct {
	ID         string    `json:"id"`
	State      string    `json:"state"`
	ReceivedAt time.Time `json:"received_at"`
}

// progressTimeout bounds asking another node about a message.
const progressTimeout = 5 * time.Second

// SentStatus reports on message id, sent by one of this node's agents. It is
// ErrNotFound if no agent here sent a message with that id.
func (m *Mailbox) SentStatus(ctx context.Context, id string) (SentStatus, error) {
	var (
		s   SentStatus
		env envelope.Envelope
	)
	out, outEnv, err := scanOutItem(m.db.QueryRow(`SELECT `+outItemColumns+` FROM outbox WHERE id = ?`, id))
	switch {
	case err == nil:
		env, s.Handover, s.SentAt = outEnv, &out, out.CreatedAt
	case errors.Is(err, sql.ErrNoRows):
		// Not for another node: maybe for an agent on this one.
		var (
			raw  []byte
			sent int64
		)
		err := m.db.QueryRow(`SELECT envelope, received_at FROM messages WHERE sender_node = ? AND id = ?`,
			m.node, id).Scan(&raw, &sent)
		if errors.Is(err, sql.ErrNoRows) {
			return SentStatus{}, ErrNotFound
		}
		if err != nil {
			return SentStatus{}, err
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			return SentStatus{}, err
		}
		s.SentAt = time.UnixMilli(sent)
	default:
		return SentStatus{}, err
	}
	s.ID, s.Kind, s.FromAgent, s.To = env.ID, string(env.Kind), env.FromAgent, env.To

	if env.Kind == envelope.Request {
		if s.Result, err = m.resultFor(env); err != nil {
			return SentStatus{}, err
		}
	}

	switch {
	case s.Handover == nil:
		// The receiver is this mailbox: nothing to ask.
		p, err := m.Progress(m.node, id)
		if err != nil {
			return SentStatus{}, err
		}
		s.Receiver = &p
	case s.Handover.State == "handed" && s.Result == nil:
		if m.transport == nil {
			s.ReceiverError = ErrRemoteNotSupported.Error()
			break
		}
		to, _ := envelope.ParseAddress(env.To) // checked when it was sent
		pctx, cancel := context.WithTimeout(ctx, progressTimeout)
		p, err := m.transport.Progress(pctx, to.Node, id)
		cancel()
		if err != nil {
			s.ReceiverError = err.Error()
		} else {
			s.Receiver = &p
		}
	}
	return s, nil
}

// resultFor finds the Result that answers req, if one has arrived. Only the
// agent req went to can answer it.
func (m *Mailbox) resultFor(req envelope.Envelope) (*ResultInfo, error) {
	to, _ := envelope.ParseAddress(req.To)
	var (
		raw      []byte
		received int64
	)
	err := m.db.QueryRow(`SELECT envelope, received_at FROM messages
		WHERE kind = 'result' AND sender_node = ? AND from_agent = ? AND json_extract(envelope, '$.reply_to') = ?
		ORDER BY seq LIMIT 1`, to.Node, to.Agent, req.ID).Scan(&raw, &received)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var res envelope.Envelope
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	return &ResultInfo{ID: res.ID, State: string(res.Body.State), ReceivedAt: time.UnixMilli(received)}, nil
}

func timePtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMilli(v.Int64)
	return &t
}
