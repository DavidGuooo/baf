package mailbox

import (
	"encoding/json"
	"time"
)

// DoorEvents nudges the Public door after mail is stored. Wake must return
// immediately; callbacks and retries never run on the mailbox's send path.
type DoorEvents interface {
	Wake()
}

// SetDoorEvents connects the door's event loop, or disconnects it with nil.
func (m *Mailbox) SetDoorEvents(events DoorEvents) {
	m.eventsMu.Lock()
	defer m.eventsMu.Unlock()
	m.events = events
}

// DoorMessage pairs a stored message with its position in the mailbox.
// The door saves its position with its delivery queue, so a crash between
// storing mail and waking the door cannot lose an event.
type DoorMessage struct {
	Sequence int64
	Item
}

// DoorSequence is the boundary for a new subscription: only later mail rings it.
func (m *Mailbox) DoorSequence() (int64, error) {
	var seq int64
	err := m.db.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM messages`).Scan(&seq)
	return seq, err
}

// DoorMessages returns a bounded batch without opening or changing any mail.
// Only Door inbox names contain a slash; local agents never appear here.
func (m *Mailbox) DoorMessages(after int64) ([]DoorMessage, error) {
	rows, err := m.db.Query(`SELECT seq, sender_node, envelope, received_at
 FROM messages WHERE seq > ? AND instr(to_agent, '/') > 0 ORDER BY seq LIMIT 100`, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []DoorMessage
	for rows.Next() {
		var m DoorMessage
		var node string
		var raw []byte
		var received int64
		if err := rows.Scan(&m.Sequence, &node, &raw, &received); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &m.Message); err != nil {
			return nil, err
		}
		m.From = node + "/" + m.Message.FromAgent
		m.ReceivedAt = time.UnixMilli(received)
		messages = append(messages, m)
	}
	return messages, rows.Err()
}
