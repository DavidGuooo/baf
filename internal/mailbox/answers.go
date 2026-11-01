package mailbox

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/DavidGuooo/baf/internal/envelope"
)

// AnswerTimeout is how long a sender waits before baf tells it no answer
// has arrived. It does not cancel the request or refuse a later Result.
const AnswerTimeout = time.Hour

// noDeadline in answer_expired_at marks a request that needs no notice: it
// was answered in time, or it was sent before baf had a deadline.
const noDeadline = 0

// expiredAt reads answer_expired_at, where noDeadline means no notice.
func expiredAt(v sql.NullInt64) *time.Time {
	if v.Int64 == noDeadline {
		return nil
	}
	return timePtr(v)
}

// expireAnswers closes unanswered requests at their sending mailbox. The
// receiver need not be awake, and handover can continue after the notice.
func (m *Mailbox) expireAnswers() error {
	var doors []string
	var errs []error
	if m.door != nil {
		var err error
		doors, err = m.DoorAgents()
		if err != nil {
			// Local senders and outbox origins do not need a policy lookup.
			// Retry locally held Door requests on the next sweep.
			errs = append(errs, err)
		}
	}
	now := m.now()
	cutoff := now.Add(-AnswerTimeout).UnixMilli()
	rows, err := m.db.Query(`SELECT 'outbox', seq, envelope, ? FROM outbox
		WHERE json_extract(envelope, '$.kind') = 'request'
		AND created_at < ? AND answer_expired_at IS NULL AND state != 'failed'
		UNION ALL
		SELECT 'messages', seq, envelope, sender_node FROM messages
		WHERE kind = 'request' AND received_at < ? AND answer_expired_at IS NULL
		AND forwarding_node = ''`, m.node, cutoff, cutoff)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	type request struct {
		table string
		seq   int64
		env   envelope.Envelope
		from  Principal
	}
	var requests []request
	for rows.Next() {
		var r request
		var raw []byte
		if err := rows.Scan(&r.table, &r.seq, &raw, &r.from.Node); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal(raw, &r.env); err != nil {
			rows.Close()
			return err
		}
		r.from.Agent = r.env.FromAgent
		if r.table == "messages" && r.from.Node != m.node && !slices.Contains(doors, r.from.String()) {
			continue // Another node's request belongs to its sending mailbox.
		}
		if r.env.Origin != "" {
			r.from, err = envelope.ParseAddress(r.env.Origin)
			if err != nil {
				rows.Close()
				return err
			}
		}
		if r.from.Agent != SystemAgent {
			requests = append(requests, r)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, r := range requests {
		told := false
		err := m.inTx(func(tx *sql.Tx) error {
			result, err := resultFor(tx, r.env, r.from)
			if err != nil {
				return err
			}
			closed := now.UnixMilli()
			if result != nil {
				closed = noDeadline // answered in time: never look at it again
			}
			// Recheck in the transaction: another sweep or failed handover
			// may have closed it since we read the candidates.
			q := `UPDATE ` + r.table + ` SET answer_expired_at = ? WHERE seq = ? AND answer_expired_at IS NULL`
			if r.table == "outbox" {
				q += ` AND state != 'failed'`
			}
			updated, err := tx.Exec(q, closed, r.seq)
			if err != nil {
				return err
			}
			n, err := updated.RowsAffected()
			if err != nil || n == 0 || result != nil {
				return err
			}
			orig := r.env
			if r.from.Node != m.node {
				// Local Door requests carry their Principal in the row,
				// rather than an origin in the stored envelope.
				orig.Origin = r.from.String()
			}
			text := fmt.Sprintf("No answer from %s within 1 hour to your request %s. It may still answer; status %s shows where it is.",
				r.env.To, r.env.ID, r.env.ID)
			if err := m.senderNoticeTx(tx, orig, text); err != nil {
				return err
			}
			told = true
			return nil
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("expire request %s: %w", r.env.ID, err))
		} else if told {
			m.signal()
		}
	}
	return errors.Join(errs...)
}
