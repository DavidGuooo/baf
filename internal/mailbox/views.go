package mailbox

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/DavidGuooo/baf/internal/envelope"
)

// Item is a stored message as an agent sees it in its inbox.
type Item struct {
	Message    envelope.Envelope `json:"message"`
	From       string            `json:"from"`  // the principal: node from the mailbox, agent claimed
	State      string            `json:"state"` // pending | running | done | inbox
	ReceivedAt time.Time         `json:"received_at"`
	Read       bool              `json:"read"`
}

// ErrNotFound means no such message for that agent.
var ErrNotFound = errors.New("no such message")

// Inbox lists messages for agentName, oldest first. Unless all is true it
// leaves out messages the agent has already read.
func (m *Mailbox) Inbox(agentName string, all bool) ([]Item, error) {
	q := `SELECT sender_node, envelope, state, received_at, read_at FROM messages WHERE to_agent = ?`
	if !all {
		q += ` AND read_at IS NULL`
	}
	rows, err := m.db.Query(q+` ORDER BY seq`, agentName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// Read returns one message for agentName and marks it read.
func (m *Mailbox) Read(agentName, id string) (Item, error) {
	row := m.db.QueryRow(`SELECT sender_node, envelope, state, received_at, read_at
		FROM messages WHERE to_agent = ? AND id = ? ORDER BY seq LIMIT 1`, agentName, id)
	it, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, ErrNotFound
	}
	if err != nil {
		return Item{}, err
	}
	if !it.Read {
		_, err = m.db.Exec(`UPDATE messages SET read_at = ? WHERE to_agent = ? AND id = ? AND read_at IS NULL`,
			m.now().UnixMilli(), agentName, id)
	}
	return it, err
}

type scanner interface{ Scan(...any) error }

func scanItem(s scanner) (Item, error) {
	var (
		node, state string
		raw         []byte
		received    int64
		readAt      sql.NullInt64
	)
	if err := s.Scan(&node, &raw, &state, &received, &readAt); err != nil {
		return Item{}, err
	}
	var env envelope.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return Item{}, err
	}
	return Item{
		Message:    env,
		From:       Principal{Node: node, Agent: env.FromAgent}.String(),
		State:      state,
		ReceivedAt: time.UnixMilli(received),
		Read:       readAt.Valid,
	}, nil
}

// RunInfo is one row of the run ledger.
type RunInfo struct {
	ID        int64      `json:"id"`
	Agent     string     `json:"agent"`
	ThreadID  string     `json:"thread_id"`
	State     string     `json:"state"`
	Detail    string     `json:"detail,omitempty"`
	MessageID string     `json:"message_id"`
	Kind      string     `json:"kind"`
	From      string     `json:"from"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// Runs lists the most recent runs, newest first.
func (m *Mailbox) Runs(limit int) ([]RunInfo, error) {
	rows, err := m.db.Query(`SELECT r.id, r.agent, r.thread_id, r.state, r.detail, m.id, m.kind,
		m.sender_node || '/' || m.from_agent, r.started_at, r.ended_at
		FROM runs r JOIN messages m ON m.seq = r.message_seq ORDER BY r.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunInfo
	for rows.Next() {
		var (
			r       RunInfo
			started int64
			ended   sql.NullInt64
		)
		err := rows.Scan(&r.ID, &r.Agent, &r.ThreadID, &r.State, &r.Detail, &r.MessageID, &r.Kind,
			&r.From, &started, &ended)
		if err != nil {
			return nil, err
		}
		r.StartedAt = time.UnixMilli(started)
		if ended.Valid {
			t := time.UnixMilli(ended.Int64)
			r.EndedAt = &t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
