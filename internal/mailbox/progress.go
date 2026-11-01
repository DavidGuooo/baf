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
	Origin   string    `json:"origin,omitempty"`
	To       string    `json:"to"` // node/agent
	StoredAt time.Time `json:"stored_at"`

	// State is the message's state here: pending (stored, its run not
	// started yet), running, done (its run ended), or inbox (an agent baf
	// doesn't run, which reads its own mail).
	State      string     `json:"state"`
	NotifiedAt *time.Time `json:"notified_at,omitempty"` // first accepted Door event
	ReadAt     *time.Time `json:"read_at,omitempty"`     // first opened: by its run, or with `baf read`

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
	return m.progress(Principal{Node: senderNode}, id)
}

func (m *Mailbox) progress(from Principal, id string) (Progress, error) {
	return m.progressVia(from, id, "")
}

// ForwardedProgress requires the Relay recorded when the message arrived.
func (m *Mailbox) ForwardedProgress(relay, origin, id string) (Progress, error) {
	from, err := envelope.ParseAddress(origin)
	if err != nil {
		return Progress{}, ErrNotFound
	}
	return m.progressVia(from, id, relay)
}

func (m *Mailbox) progressVia(from Principal, id, relay string) (Progress, error) {
	var (
		p                       Progress
		received                int64
		readAt, notifiedAt      sql.NullInt64
		runState, thread, doing sql.NullString
		started, ended, doingAt sql.NullInt64
	)
	err := m.db.QueryRow(`SELECT json_extract(m.envelope, '$.to'), COALESCE(json_extract(m.envelope, '$.origin'), ''), m.state, m.received_at, m.read_at, m.notified_at,
			r.state, r.thread_id, r.started_at, r.ended_at, r.doing, r.doing_at
		FROM messages m LEFT JOIN runs r ON r.message_seq = m.seq
		WHERE m.sender_node = ? AND m.id = ?
		AND (? = '' OR m.from_agent = ?)
		AND (? = '' OR m.forwarding_node = ?)
		ORDER BY r.id DESC LIMIT 1`, from.Node, id, from.Agent, from.Agent, relay, relay).Scan(
		&p.To, &p.Origin, &p.State, &received, &readAt, &notifiedAt, &runState, &thread, &started, &ended, &doing, &doingAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Progress{}, ErrNotFound
	}
	if err != nil {
		return Progress{}, err
	}
	p.ID = id
	p.NotifiedAt = timePtr(notifiedAt)
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
	Origin    string    `json:"origin,omitempty"`
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

	// AnswerExpiredAt is when baf told the sender no answer had arrived.
	AnswerExpiredAt *time.Time `json:"answer_expired_at,omitempty"`
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
	return m.sentStatus(ctx, Principal{Node: m.node}, id)
}

func (m *Mailbox) sentStatus(ctx context.Context, from Principal, id string) (SentStatus, error) {
	var (
		s   SentStatus
		env envelope.Envelope
	)
	// A Door agent may see only its own outbox entries, even if another
	// sender chose the same id for local mail.
	origin := ""
	if from.Agent != "" {
		origin = from.String()
	}
	out, outEnv, err := scanOutItem(m.db.QueryRow(`SELECT `+outItemColumns+` FROM outbox
		WHERE id = ? AND (? = '' OR json_extract(envelope, '$.origin') = ?)`, id, from.Agent, origin))
	switch {
	case err == nil:
		env, s.Handover, s.SentAt = outEnv, &out, out.CreatedAt
		var expired sql.NullInt64
		if err := m.db.QueryRow(`SELECT answer_expired_at FROM outbox WHERE id = ?`, id).Scan(&expired); err != nil {
			return SentStatus{}, err
		}
		s.AnswerExpiredAt = expiredAt(expired)
	case errors.Is(err, sql.ErrNoRows):
		// Not for another node: maybe for an agent on this one.
		var (
			raw     []byte
			sent    int64
			expired sql.NullInt64
		)
		err := m.db.QueryRow(`SELECT envelope, received_at, answer_expired_at FROM messages WHERE sender_node = ? AND id = ?
			AND (? = '' OR from_agent = ?)`, from.Node, id, from.Agent, from.Agent).Scan(&raw, &sent, &expired)
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
		s.AnswerExpiredAt = expiredAt(expired)
	default:
		return SentStatus{}, err
	}
	s.ID, s.Kind, s.FromAgent, s.To = env.ID, string(env.Kind), env.FromAgent, env.To
	from.Agent = env.FromAgent
	s.Origin = env.Origin
	if env.Origin != "" {
		from, _ = envelope.ParseAddress(env.Origin)
	}

	if env.Kind == envelope.Request {
		if s.Result, err = m.resultFor(env, from); err != nil {
			return SentStatus{}, err
		}
	}

	switch {
	case s.Handover == nil:
		// The receiver is this mailbox: nothing to ask.
		p, err := m.progress(from, id)
		if err != nil {
			return SentStatus{}, err
		}
		s.Receiver = &p
	case s.Handover.State == "handed" && s.Result == nil:
		if m.transport == nil {
			s.ReceiverError = ErrRemoteNotSupported.Error()
			break
		}
		pctx, cancel := context.WithTimeout(ctx, progressTimeout)
		p, err := m.transport.Progress(pctx, s.Handover.Via, id, env.Origin)
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
func (m *Mailbox) resultFor(req envelope.Envelope, from Principal) (*ResultInfo, error) {
	return resultFor(m.db, req, from)
}

// A transaction uses the same lookup so an answer cannot arrive between
// checking for it and storing the no-answer notice.
func resultFor(db interface {
	QueryRow(string, ...any) *sql.Row
}, req envelope.Envelope, from Principal) (*ResultInfo, error) {
	to, _ := envelope.ParseAddress(req.To)
	var (
		raw      []byte
		received int64
	)
	err := db.QueryRow(`SELECT envelope, received_at FROM messages
		WHERE kind = 'result' AND sender_node = ? AND from_agent = ? AND json_extract(envelope, '$.reply_to') = ?
		AND json_extract(envelope, '$.to') = ?
		ORDER BY seq LIMIT 1`, to.Node, to.Agent, req.ID, from.String()).Scan(&raw, &received)
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
