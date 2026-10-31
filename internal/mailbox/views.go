package mailbox

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	Files      []FileInfo        `json:"files,omitempty"` // the message's files, where they are on this machine
}

// ErrNotFound means no message has that id.
var ErrNotFound = errors.New("no such message")

// ErrNotForYou means the message exists but is addressed to another agent.
var ErrNotForYou = errors.New("that message is not addressed to you")

// Inbox lists messages on this node, oldest first: those addressed to
// agentName, or to every agent if agentName is empty. Unless all is true it
// leaves out messages already read.
func (m *Mailbox) Inbox(agentName string, all bool) ([]Item, error) {
	q := `SELECT sender_node, envelope, state, received_at, read_at FROM messages WHERE (? = '' OR to_agent = ?)`
	if !all {
		q += ` AND read_at IS NULL`
	}
	rows, err := m.db.Query(q+` ORDER BY seq`, agentName, agentName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []Item
	for rows.Next() {
		it, err := m.scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// Read returns the message with this id and marks it read. The id alone
// finds it. If agentName is not empty, Read opens it only when it is
// addressed to that agent, so an agent can't open another's mail by mistake.
// (Agents sharing a node could still read the database; this is a guard
// against mistakes, not a security boundary.)
func (m *Mailbox) Read(agentName, id string) (Item, error) {
	var (
		seq     int64
		toAgent string
	)
	err := m.db.QueryRow(`SELECT seq, to_agent FROM messages WHERE id = ? AND (? = '' OR to_agent = ?)
		ORDER BY seq LIMIT 1`, id, agentName, agentName).Scan(&seq, &toAgent)
	if errors.Is(err, sql.ErrNoRows) {
		// Keep the helpful local error without selecting another agent's
		// message ahead of ours when two senders chose the same id.
		err = m.db.QueryRow(`SELECT to_agent FROM messages WHERE id = ? LIMIT 1`, id).Scan(&toAgent)
		if errors.Is(err, sql.ErrNoRows) {
			return Item{}, ErrNotFound
		}
		if err == nil {
			return Item{}, fmt.Errorf("%w: %s is for %s, not %s", ErrNotForYou, id, toAgent, agentName)
		}
	}
	if err != nil {
		return Item{}, err
	}
	if agentName != "" && agentName != toAgent {
		return Item{}, fmt.Errorf("%w: %s is for %s, not %s", ErrNotForYou, id, toAgent, agentName)
	}
	it, err := m.scanItem(m.db.QueryRow(`SELECT sender_node, envelope, state, received_at, read_at
		FROM messages WHERE seq = ?`, seq))
	if err != nil {
		return Item{}, err
	}
	if !it.Read {
		_, err = m.db.Exec(`UPDATE messages SET read_at = ? WHERE seq = ? AND read_at IS NULL`, m.now().UnixMilli(), seq)
	}
	return it, err
}

type scanner interface{ Scan(...any) error }

func (m *Mailbox) scanItem(s scanner) (Item, error) {
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
	var files []FileInfo
	for _, f := range env.Body.Files {
		files = append(files, FileInfo{File: f, Path: m.filePath(node, env.ID, f.Name)})
	}
	return Item{
		Files:      files,
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
	Doing     string     `json:"doing,omitempty"` // what the agent last started doing
}

// Runs lists the most recent runs, newest first.
func (m *Mailbox) Runs(limit int) ([]RunInfo, error) {
	rows, err := m.db.Query(`SELECT r.id, r.agent, r.thread_id, r.state, r.detail, m.id, m.kind,
		m.sender_node || '/' || m.from_agent, r.started_at, r.ended_at, r.doing
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
			&r.From, &started, &ended, &r.Doing)
		if err != nil {
			return nil, err
		}
		r.StartedAt = time.UnixMilli(started)
		r.EndedAt = timePtr(ended)
		out = append(out, r)
	}
	return out, rows.Err()
}
