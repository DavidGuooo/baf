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
	"slices"
	"strings"
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

	// Progress asks about a message we handed over. Origin scopes a Door
	// agent's id; it is empty for this node's own agents (ADR-0006).
	Progress(ctx context.Context, node, id, origin string) (Progress, error)

	// PutFile hands the bytes of f to node's mailbox ahead of a message to
	// to that names f, with the same origin as the push (ADR-0007).
	// A *Rejection means the receiver refused the file for good; any other
	// error means try again later.
	PutFile(ctx context.Context, node, to, origin string, f envelope.File, body io.Reader) error
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

// pushTimeout plus the default RetryDelay makes a try every 30 seconds while
// the receiver doesn't answer.
const pushTimeout = 25 * time.Second

// enqueueTx puts env in the outbox for the node in to. Sending the same id
// twice queues it once.
func (m *Mailbox) enqueueTx(tx *sql.Tx, env envelope.Envelope, to envelope.Address) (Receipt, error) {
	var created int64
	var rawOld []byte
	err := tx.QueryRow(`SELECT created_at, envelope FROM outbox WHERE id = ?`, env.ID).Scan(&created, &rawOld)
	if err == nil {
		var old envelope.Envelope
		if err := json.Unmarshal(rawOld, &old); err != nil {
			return Receipt{}, err
		}
		if old.Origin != env.Origin || old.FromAgent != env.FromAgent {
			return Receipt{}, reject("message id %q is already used by another agent; choose a new id", env.ID)
		}
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
	sender := env.FromAgent
	if env.Origin != "" {
		sender = env.Origin
	}
	return env.To + "\x00" + sender + "\x00" + env.FromThread
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
	notice, err := m.attemptHandover(ctx, &o)
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
		slog.Info("handover failed; will retry", "message", o.env.ID, "to", o.env.To,
			"attempt", attempts, "in", m.retryDelay, "err", err)
		err = m.inTx(func(tx *sql.Tx) error {
			_, err2 := tx.Exec(`UPDATE outbox SET attempts = ?, rung = ?, last_error = ?, next_try_at = ? WHERE seq = ?`,
				attempts, o.rung, err.Error(), m.now().Add(m.retryDelay).UnixMilli(), o.seq)
			return err2
		})
	}
	if err != nil {
		slog.Error("record handover", "message", o.env.ID, "err", err)
	}
}

// attemptHandover refreshes the Relay route after a failed attempt, keeping
// the destination in the envelope and the node in the outbox separate.
func (m *Mailbox) attemptHandover(ctx context.Context, o *outgoing) (string, error) {
	to, _ := envelope.ParseAddress(o.env.To)
	if o.node != to.Node {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		node, err := m.relay(rctx, o.attempts > 0)
		cancel()
		if err != nil {
			return "", fmt.Errorf("finding the Relay for %s: %w", o.env.To, err)
		}
		if node != o.node {
			o.node, o.rung = node, false
			if err := m.inTx(func(tx *sql.Tx) error {
				_, err := tx.Exec(`UPDATE outbox SET to_node = ?, rung = 0 WHERE seq = ?`, node, o.seq)
				return err
			}); err != nil {
				return "", err
			}
		}
	}
	// Ring first: the ring may be what brings a sleeping node back online.
	// A node with a doorbell gets no files or message until a ring gets through.
	// Retry a failed ring; once it works, retries of the push don't ring again.
	if !o.rung && m.doorbell != nil {
		err := m.doorbell.Ring(ctx, o.node, o.env)
		switch {
		case err == nil, errors.Is(err, ErrNoDoorbell):
			o.rung = true
		default:
			return "", fmt.Errorf("doorbell did not ring: %w", err)
		}
	}

	// The files go first: the receiver stores the message only once it holds
	// every file the message names (ADR-0007).
	notice, err := m.handOverFiles(ctx, *o)
	if err == nil {
		pctx, cancel := context.WithTimeout(ctx, pushTimeout)
		_, err = m.transport.Push(pctx, o.node, o.env)
		cancel()
	}
	return notice, err
}

// handOverFiles hands the receiver the bytes of each file o names, once per
// hash. It returns a *Rejection when o can never go, because the receiver
// refused a file or a file's bytes are gone from this machine, with the text
// of the sender's failure notice. Any other error means try again later.
func (m *Mailbox) handOverFiles(ctx context.Context, o outgoing) (notice string, err error) {
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
		err = m.transport.PutFile(fctx, o.node, o.env.To, o.env.Origin, f, file)
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
	if err := m.senderNoticeTx(tx, orig, text); err != nil {
		// The sending agent no longer exists here; nobody to tell.
		slog.Error("deliver failure notice", "message", orig.ID, "err", err)
	}
	return nil
}

// senderNoticeTx also serves the answer deadline. Its caller records the
// notice and the reason for it together, before waking the sender.
func (m *Mailbox) senderNoticeTx(tx *sql.Tx, orig envelope.Envelope, text string) error {
	n := envelope.Envelope{
		V:         envelope.Version,
		ID:        envelope.NewID(),
		Kind:      envelope.Notice,
		To:        Principal{Node: m.node, Agent: orig.FromAgent}.String(),
		FromAgent: SystemAgent,
		ToThread:  orig.FromThread,
		Body:      envelope.Body{Text: text + "\n--- your original message ---\n" + truncate(orig.Body.Text, 2000)},
	}
	var err error
	if orig.Origin != "" {
		n.To = orig.Origin
		_, err = m.storeInboxTx(tx, n, Principal{Node: m.node, Agent: SystemAgent}, n.To, nil, false)
	} else {
		_, err = m.storeTx(tx, n, Principal{Node: m.node, Agent: SystemAgent}, false)
	}
	return err
}

// OutItem is one message in the outbox, for `baf outbox`.
type OutItem struct {
	ID        string     `json:"id"`
	To        string     `json:"to"`
	Kind      string     `json:"kind"`
	FromAgent string     `json:"from_agent"`
	Origin    string     `json:"origin,omitempty"`
	Via       string     `json:"via,omitempty"`
	State     string     `json:"state"` // waiting | handed | failed
	Attempts  int        `json:"attempts"`
	LastError string     `json:"last_error,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	NextTry   time.Time  `json:"next_try_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	DoneAt    *time.Time `json:"done_at,omitempty"` // when it was handed over, or failed
}

const outItemColumns = `to_node, envelope, state, attempts, last_error, created_at, next_try_at, expires_at, done_at`

// scanOutItem reads one outbox row selected with outItemColumns.
func scanOutItem(s scanner) (OutItem, envelope.Envelope, error) {
	var (
		it                     OutItem
		env                    envelope.Envelope
		raw                    []byte
		created, next, expires int64
		done                   sql.NullInt64
	)
	if err := s.Scan(&it.Via, &raw, &it.State, &it.Attempts, &it.LastError, &created, &next, &expires, &done); err != nil {
		return OutItem{}, env, err
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return OutItem{}, env, err
	}
	it.ID, it.To, it.Kind, it.FromAgent = env.ID, env.To, string(env.Kind), env.FromAgent
	it.Origin = env.Origin
	it.CreatedAt, it.NextTry, it.ExpiresAt = time.UnixMilli(created), time.UnixMilli(next), time.UnixMilli(expires)
	it.DoneAt = timePtr(done)
	return it, env, nil
}

// Outbox lists outgoing messages, newest first. Unless all is true it lists
// only those still waiting.
func (m *Mailbox) Outbox(all bool, limit int) ([]OutItem, error) {
	return m.OutboxAs("", all, limit)
}

// OutboxAs filters by the agent that sent the mail. A Door agent's node is
// its vendor, so mail it sent to this Relay counts as handed over too.
func (m *Mailbox) OutboxAs(agentName string, all bool, limit int) ([]OutItem, error) {
	var from Principal
	if strings.Contains(agentName, "/") && m.door != nil {
		peer, err := m.doorPeer()
		if err != nil {
			return nil, err
		}
		if peer.IsRelay() {
			if !slices.Contains(peer.DoorAgents(), agentName) {
				return nil, reject("the Tailscale policy does not list %q as a Door agent on this Relay", agentName)
			}
			from, _ = envelope.ParseAddress(agentName) // DoorAgents returns valid addresses.
		}
	}
	q := `SELECT ` + outItemColumns + ` FROM outbox WHERE (? = '' OR
		(COALESCE(json_extract(envelope, '$.origin'), '') = '' AND json_extract(envelope, '$.from_agent') = ?) OR
		json_extract(envelope, '$.origin') = ?)`
	if !all {
		q += ` AND state = 'waiting'`
	}
	rows, err := m.db.Query(q+` ORDER BY seq DESC LIMIT ?`, agentName, agentName, agentName, limit)
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close() // Release the connection before reading locally held mail.
	if from.Node == "" || !all {
		return out, nil
	}
	rows, err = m.db.Query(`SELECT envelope, received_at FROM messages
		WHERE sender_node = ? AND from_agent = ?
		AND substr(json_extract(envelope, '$.to'), 1, instr(json_extract(envelope, '$.to'), '/') - 1) != ?
		ORDER BY received_at DESC, seq DESC LIMIT ?`, from.Node, from.Agent, from.Node, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		var received int64
		if err := rows.Scan(&raw, &received); err != nil {
			return nil, err
		}
		var env envelope.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, err
		}
		stored := time.UnixMilli(received)
		out = append(out, OutItem{ID: env.ID, To: env.To, Kind: string(env.Kind),
			FromAgent: from.Agent, Origin: agentName, Via: m.node, State: "handed",
			CreatedAt: stored, DoneAt: &stored})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortStableFunc(out, func(a, b OutItem) int { return b.CreatedAt.Compare(a.CreatedAt) })
	if limit >= 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
