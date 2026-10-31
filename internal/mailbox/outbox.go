package mailbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/DavidGuooo/baf/internal/envelope"
)

// Transport hands messages to other nodes' mailboxes (tailnet HTTP in
// production, in-memory in tests).
type Transport interface {
	// Push hands env to node's mailbox and returns its receipt. A
	// *Rejection means the receiver refused the message for good; any other
	// error means try again later.
	Push(ctx context.Context, node string, env envelope.Envelope) (Receipt, error)

	// Known returns an error if there is no machine called node to send to.
	Known(ctx context.Context, node string) error

	// Progress asks node's mailbox about message id, which this node handed
	// to it (ADR-0006).
	Progress(ctx context.Context, node, id string) (Progress, error)

	// PutFile hands the bytes of f to node's mailbox ahead of a message to
	// toAgent that names f (ADR-0007). A *Rejection means the receiver
	// refused the file for good; any other error means try again later.
	PutFile(ctx context.Context, node, toAgent string, f envelope.File, body io.Reader) error
}

// Doorbell wakes another node's agents so they look at their mailbox, for
// nodes where nothing else would (Grok's Bridge bot). A ring carries no
// authority; it only says "look now".
type Doorbell interface {
	// Ring rings node's doorbell about env. It returns ErrNoDoorbell if this
	// node holds no doorbell key for that node.
	Ring(ctx context.Context, node string, env envelope.Envelope) error
}

// ErrNoDoorbell means there is no doorbell to ring for that node.
var ErrNoDoorbell = errors.New("no doorbell for that node")

const pushTimeout = 20 * time.Second

// enqueueTx puts env in the outbox for the node in to. Sending the same id
// twice queues it once.
func (m *Mailbox) enqueueTx(tx *sql.Tx, env envelope.Envelope, to envelope.Address) (Receipt, error) {
	var created int64
	err := tx.QueryRow(`SELECT created_at FROM outbox WHERE id = ?`, env.ID).Scan(&created)
	if err == nil {
		return Receipt{ID: env.ID, StoredAt: time.UnixMilli(created), Duplicate: true, Queued: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, err
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return Receipt{}, err
	}
	now := m.now()
	_, err = tx.Exec(`INSERT INTO outbox (id, to_node, queue, envelope, state, created_at, next_try_at, expires_at)
		VALUES (?, ?, ?, ?, 'waiting', ?, ?, ?)`,
		env.ID, to.Node, queueKey(env), raw, now.UnixMilli(), now.UnixMilli(), now.Add(m.giveUpAfter).UnixMilli())
	if err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: env.ID, StoredAt: now, Queued: true}, nil
}

// queueKey names the queue a message waits in. Messages from one sender's
// thread to one receiver are handed over strictly in order.
func queueKey(env envelope.Envelope) string {
	return env.To + "\x00" + env.FromAgent + "\x00" + env.FromThread
}

// outgoing is a message waiting in the outbox.
type outgoing struct {
	seq       int64
	node      string
	queue     string
	env       envelope.Envelope
	attempts  int
	rung      bool
	lastError string
	nextTry   time.Time
	expires   time.Time
}

// runOutbox pushes waiting messages until ctx is cancelled. Each queue
// pushes only its oldest message, and the next one only after a receipt.
func (m *Mailbox) runOutbox(ctx context.Context) {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		busy = map[string]bool{} // queues with a push in flight
	)
	defer wg.Wait()
	for {
		heads, err := m.outboxHeads()
		if err != nil {
			slog.Error("read outbox", "err", err)
		}
		now := m.now()
		wait := time.Minute // look again at least this often
		for _, h := range heads {
			mu.Lock()
			inFlight := busy[h.queue]
			mu.Unlock()
			switch {
			case inFlight:
			case !now.Before(h.expires):
				m.giveUp(h)
			case now.Before(h.nextTry):
				wait = min(wait, h.nextTry.Sub(now))
			default:
				mu.Lock()
				busy[h.queue] = true
				mu.Unlock()
				wg.Add(1)
				go func() {
					defer wg.Done()
					m.handover(ctx, h)
					mu.Lock()
					delete(busy, h.queue)
					mu.Unlock()
					m.signal() // the next message in this queue may go now
				}()
			}
		}
		timer := time.NewTimer(max(wait, 10*time.Millisecond))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-m.outboxWake:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// outboxHeads returns the oldest waiting message of every queue.
func (m *Mailbox) outboxHeads() ([]outgoing, error) {
	rows, err := m.db.Query(`SELECT o.seq, o.to_node, o.queue, o.envelope, o.attempts, o.rung, o.last_error,
			o.next_try_at, o.expires_at
		FROM outbox o
		WHERE o.state = 'waiting'
		  AND o.seq = (SELECT MIN(seq) FROM outbox WHERE queue = o.queue AND state = 'waiting')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []outgoing
	for rows.Next() {
		var (
			o             outgoing
			raw           []byte
			next, expires int64
		)
		if err := rows.Scan(&o.seq, &o.node, &o.queue, &raw, &o.attempts, &o.rung, &o.lastError, &next, &expires); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &o.env); err != nil {
			return nil, err
		}
		o.nextTry, o.expires = time.UnixMilli(next), time.UnixMilli(expires)
		out = append(out, o)
	}
	return out, rows.Err()
}

// handover makes one attempt to hand o to its node and records the result.
func (m *Mailbox) handover(ctx context.Context, o outgoing) {
	// Ring first: the ring may be what brings a sleeping node back online.
	// Only the first attempt rings, unless no ring has got through yet.
	if !o.rung && m.doorbell != nil {
		err := m.doorbell.Ring(ctx, o.node, o.env)
		switch {
		case err == nil, errors.Is(err, ErrNoDoorbell):
			o.rung = true
		default:
			slog.Warn("doorbell did not ring", "node", o.node, "message", o.env.ID, "err", err)
		}
	}

	// The files go first: the receiver stores the message only once it holds
	// every file the message names (ADR-0007).
	notice, err := m.handOverFiles(ctx, o)
	if err == nil {
		pctx, cancel := context.WithTimeout(ctx, pushTimeout)
		_, err = m.transport.Push(pctx, o.node, o.env)
		cancel()
	}
	if ctx.Err() != nil {
		return // baf is stopping; this attempt doesn't count
	}

	var rej *Rejection
	switch {
	case err == nil:
		slog.Info("handed over", "message", o.env.ID, "to", o.env.To, "attempts", o.attempts+1)
		err = m.inTx(func(tx *sql.Tx) error {
			_, err := tx.Exec(`UPDATE outbox SET state = 'handed', attempts = attempts + 1, rung = ?, done_at = ?,
				last_error = '' WHERE seq = ?`, o.rung, m.now().UnixMilli(), o.seq)
			return err
		})
	case errors.As(err, &rej):
		// The receiver answered and refused. Later messages in the queue
		// still go: order is kept among the messages it accepts.
		slog.Warn("refused by receiver", "message", o.env.ID, "to", o.env.To, "reason", rej.Reason)
		err = m.inTx(func(tx *sql.Tx) error {
			if err := m.failTx(tx, o.seq, rej.Reason); err != nil {
				return err
			}
			if notice == "" {
				notice = fmt.Sprintf("%s refused your %s %s: %s", o.env.To, o.env.Kind, o.env.ID, rej.Reason)
			}
			return m.failureNoticeTx(tx, o.env, notice)
		})
	default:
		attempts := o.attempts + 1
		delay := min(m.retryBase<<min(attempts-1, 20), m.retryMax)
		slog.Info("handover failed; will retry", "message", o.env.ID, "to", o.env.To,
			"attempt", attempts, "in", delay, "err", err)
		err = m.inTx(func(tx *sql.Tx) error {
			_, err2 := tx.Exec(`UPDATE outbox SET attempts = ?, rung = ?, last_error = ?, next_try_at = ? WHERE seq = ?`,
				attempts, o.rung, err.Error(), m.now().Add(delay).UnixMilli(), o.seq)
			return err2
		})
	}
	if err != nil {
		slog.Error("record handover", "message", o.env.ID, "err", err)
	}
}

// handOverFiles hands the receiver the bytes of each file o names, once per
// hash. It returns a *Rejection when o can never go, because the receiver
// refused a file or a file's bytes are gone from this machine, with the text
// of the sender's failure notice. Any other error means try again later.
func (m *Mailbox) handOverFiles(ctx context.Context, o outgoing) (notice string, err error) {
	to, _ := envelope.ParseAddress(o.env.To)
	sent := map[string]bool{}
	for _, f := range o.env.Body.Files {
		if sent[f.SHA256] {
			continue
		}
		sent[f.SHA256] = true
		file, err := m.openFile(f.SHA256)
		if errors.Is(err, os.ErrNotExist) {
			notice = fmt.Sprintf("File %s of your %s %s to %s is no longer on this machine; send it again.",
				f.Name, o.env.Kind, o.env.ID, o.env.To)
			return notice, &Rejection{Reason: notice}
		}
		if err != nil {
			return "", fmt.Errorf("handing over %s: %w", f.Name, err)
		}
		// Allow for a slow link: ten seconds per started MB (100 KB/s) on top of a
		// push's time. Tailscale's DERP relays carried about 300 KB/s to grokbot.
		fctx, cancel := context.WithTimeout(ctx, pushTimeout+time.Duration((f.Size+999_999)/1_000_000)*10*time.Second)
		err = m.transport.PutFile(fctx, o.node, to.Agent, f, file)
		cancel()
		file.Close()
		var rej *Rejection
		if errors.As(err, &rej) {
			notice = fmt.Sprintf("%s refused file %s of your %s %s: %s", o.env.To, f.Name, o.env.Kind, o.env.ID, rej.Reason)
			return notice, rej
		}
		if err != nil {
			return "", fmt.Errorf("handing over %s: %w", f.Name, err)
		}
	}
	return "", nil
}

// giveUp ends a queue whose oldest message ran out of time: that message and
// every later one in the queue fail, each with a notice to its sender.
func (m *Mailbox) giveUp(o outgoing) {
	err := m.inTx(func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT seq, envelope FROM outbox WHERE queue = ? AND state = 'waiting' AND seq > ? ORDER BY seq`,
			o.queue, o.seq)
		if err != nil {
			return err
		}
		type later struct {
			seq int64
			env envelope.Envelope
		}
		var rest []later
		for rows.Next() {
			var (
				l   later
				raw []byte
			)
			if err := rows.Scan(&l.seq, &raw); err != nil {
				rows.Close()
				return err
			}
			if err := json.Unmarshal(raw, &l.env); err != nil {
				rows.Close()
				return err
			}
			rest = append(rest, l)
		}
		rows.Close()

		lastError := o.lastError
		if lastError == "" {
			lastError = "no attempt got through"
		}
		reason := fmt.Sprintf("gave up after %s (last error: %s)", m.giveUpAfter, lastError)
		if err := m.failTx(tx, o.seq, reason); err != nil {
			return err
		}
		text := fmt.Sprintf("Delivery of your %s %s to %s could not be confirmed: baf %s. "+
			"It may or may not have arrived. Sending it again sends a new message, so the work may run twice.",
			o.env.Kind, o.env.ID, o.env.To, reason)
		if err := m.failureNoticeTx(tx, o.env, text); err != nil {
			return err
		}
		for _, l := range rest {
			if err := m.failTx(tx, l.seq, "an earlier message in the same thread was not delivered"); err != nil {
				return err
			}
			text := fmt.Sprintf("Your %s %s to %s was not sent, because the earlier message %s in the same thread "+
				"could not be delivered and baf keeps each thread's messages in order.",
				l.env.Kind, l.env.ID, l.env.To, o.env.ID)
			if err := m.failureNoticeTx(tx, l.env, text); err != nil {
				return err
			}
		}
		slog.Warn("gave up on handover", "message", o.env.ID, "to", o.env.To, "later_failed", len(rest))
		return nil
	})
	if err != nil {
		slog.Error("give up on handover", "message", o.env.ID, "err", err)
	}
	m.signal()
}

func (m *Mailbox) failTx(tx *sql.Tx, seq int64, reason string) error {
	_, err := tx.Exec(`UPDATE outbox SET state = 'failed', last_error = ?, done_at = ? WHERE seq = ?`,
		reason, m.now().UnixMilli(), seq)
	return err
}

// failureNoticeTx tells the agent that sent orig that it was not delivered.
// The notice reaches the agent like any message, in the thread orig came
// from, so a Codex agent is resumed in that thread.
func (m *Mailbox) failureNoticeTx(tx *sql.Tx, orig envelope.Envelope, text string) error {
	n := envelope.Envelope{
		V:         envelope.Version,
		ID:        envelope.NewID(),
		Kind:      envelope.Notice,
		To:        Principal{Node: m.node, Agent: orig.FromAgent}.String(),
		FromAgent: SystemAgent,
		ToThread:  orig.FromThread,
		Body:      envelope.Body{Text: text + "\n--- your original message ---\n" + truncate(orig.Body.Text, 2000)},
	}
	if _, err := m.storeTx(tx, n, Principal{Node: m.node, Agent: SystemAgent}, false); err != nil {
		// The sending agent no longer exists here; nobody to tell.
		slog.Error("deliver failure notice", "to", n.To, "err", err)
	}
	return nil
}

// OutItem is one message in the outbox, for `baf outbox`.
type OutItem struct {
	ID        string     `json:"id"`
	To        string     `json:"to"`
	Kind      string     `json:"kind"`
	FromAgent string     `json:"from_agent"`
	State     string     `json:"state"` // waiting | handed | failed
	Attempts  int        `json:"attempts"`
	LastError string     `json:"last_error,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	NextTry   time.Time  `json:"next_try_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	DoneAt    *time.Time `json:"done_at,omitempty"` // when it was handed over, or failed
}

const outItemColumns = `envelope, state, attempts, last_error, created_at, next_try_at, expires_at, done_at`

// scanOutItem reads one outbox row selected with outItemColumns.
func scanOutItem(s scanner) (OutItem, envelope.Envelope, error) {
	var (
		it                     OutItem
		env                    envelope.Envelope
		raw                    []byte
		created, next, expires int64
		done                   sql.NullInt64
	)
	if err := s.Scan(&raw, &it.State, &it.Attempts, &it.LastError, &created, &next, &expires, &done); err != nil {
		return OutItem{}, env, err
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return OutItem{}, env, err
	}
	it.ID, it.To, it.Kind, it.FromAgent = env.ID, env.To, string(env.Kind), env.FromAgent
	it.CreatedAt, it.NextTry, it.ExpiresAt = time.UnixMilli(created), time.UnixMilli(next), time.UnixMilli(expires)
	it.DoneAt = timePtr(done)
	return it, env, nil
}

// Outbox lists outgoing messages, newest first. Unless all is true it lists
// only those still waiting.
func (m *Mailbox) Outbox(all bool, limit int) ([]OutItem, error) {
	q := `SELECT ` + outItemColumns + ` FROM outbox`
	if !all {
		q += ` WHERE state = 'waiting'`
	}
	rows, err := m.db.Query(q+` ORDER BY seq DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutItem
	for rows.Next() {
		it, _, err := scanOutItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
