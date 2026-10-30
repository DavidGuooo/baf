// Package mailbox is the durable store on an agent's own node (GLOSSARY:
// Mailbox). It stores messages, drops retried duplicates, checks which
// threads a sender may continue, hands messages to agents one run at a time
// per thread, and answers every Request with a Result when its run ends.
//
// A message between agents on the same node moves inside the mailbox. One
// for another node waits in this mailbox's outbox until that node's mailbox
// gives a receipt (outbox.go).
package mailbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/DavidGuooo/baf/internal/agent"
	"github.com/DavidGuooo/baf/internal/envelope"
)

// ThreadTTL is how long a shared thread stays usable without any message on it.
const ThreadTTL = 30 * 24 * time.Hour

// Principal is who the mailbox believes sent a message. For a local sender
// it is this node plus the agent's name; for a remote one, the node comes
// from Tailscale.
type Principal = envelope.Address

// Receipt proves the mailbox durably stored a message, and nothing more.
type Receipt struct {
	ID        string    `json:"id"`
	StoredAt  time.Time `json:"stored_at"`
	Duplicate bool      `json:"duplicate,omitempty"` // already stored by an earlier try

	// Queued means the message is for another node and waits in this
	// node's outbox until that node's mailbox stores it.
	Queued bool `json:"queued,omitempty"`
}

// Rejection is a message the mailbox refused to store. Reason is written for
// the sending agent to read.
type Rejection struct{ Reason string }

func (r *Rejection) Error() string { return r.Reason }

func reject(format string, args ...any) error {
	return &Rejection{Reason: fmt.Sprintf(format, args...)}
}

// ErrRemoteNotSupported is returned for a message to another node when the
// mailbox was opened without a Transport.
var ErrRemoteNotSupported = errors.New("this mailbox cannot send to other machines")

// SystemAgent is the sender name on notices baf itself writes, such as a
// delivery-failure notice. No configured agent may use it.
const SystemAgent = "baf"

// AnyAgent, as a key in Options.Agents, gives every other valid agent name
// on this node an inbox. It suits nodes whose agents come and go outside
// baf, like the bots on grokbot: who may send to them is still decided by
// the Tailscale policy, and Bridge refuses names that are not real bots.
const AnyAgent = "*"

// DefaultGiveUpAfter is how long the outbox keeps trying to hand a message over.
const DefaultGiveUpAfter = 24 * time.Hour

// Options configures a Mailbox.
type Options struct {
	Path string // SQLite file
	Node string // this node's name, as other nodes address it

	// Agents served by this mailbox. A nil adapter means the agent only has
	// an inbox (for example "me", the owner at a terminal). The key AnyAgent
	// (with a nil adapter) gives every other name an inbox.
	Agents map[string]agent.Adapter

	// RunEnv returns extra environment for an agent's run. Optional.
	RunEnv func(agentName string) []string

	// Transport pushes messages to other nodes. Without one, the mailbox
	// only delivers between its own agents.
	Transport Transport

	// Doorbell wakes another node's agents. Optional.
	Doorbell Doorbell

	GiveUpAfter time.Duration // default DefaultGiveUpAfter
	RetryBase   time.Duration // first retry delay, doubling up to RetryMax; default 5s
	RetryMax    time.Duration // default 2 minutes

	Now func() time.Time // for tests; defaults to time.Now
}

// Mailbox is safe for concurrent use.
type Mailbox struct {
	db          *sql.DB
	node        string
	agents      map[string]agent.Adapter
	runEnv      func(string) []string
	now         func() time.Time
	transport   Transport
	doorbell    Doorbell
	giveUpAfter time.Duration
	retryBase   time.Duration
	retryMax    time.Duration

	wake       chan struct{} // nudges Run to look for claimable messages
	outboxWake chan struct{} // nudges the outbox to look for messages to push
}

// Open opens the mailbox and settles anything a previous crash left behind.
func Open(opts Options) (*Mailbox, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.RunEnv == nil {
		opts.RunEnv = func(string) []string { return nil }
	}
	if opts.GiveUpAfter <= 0 {
		opts.GiveUpAfter = DefaultGiveUpAfter
	}
	if opts.RetryBase <= 0 {
		opts.RetryBase = 5 * time.Second
	}
	if opts.RetryMax <= 0 {
		opts.RetryMax = 2 * time.Minute
	}
	if _, ok := opts.Agents[SystemAgent]; ok {
		return nil, fmt.Errorf("%q is reserved for baf's own notices; rename that agent", SystemAgent)
	}
	if a, ok := opts.Agents[AnyAgent]; ok && a != nil {
		return nil, fmt.Errorf("%q can only give inboxes; it cannot run an agent", AnyAgent)
	}
	if _, err := envelope.ParseAddress(opts.Node + "/x"); err != nil {
		return nil, fmt.Errorf("bad node name %q: %w", opts.Node, err)
	}
	db, err := openDB(opts.Path)
	if err != nil {
		return nil, err
	}
	m := &Mailbox{
		db:          db,
		node:        opts.Node,
		agents:      opts.Agents,
		runEnv:      opts.RunEnv,
		now:         opts.Now,
		transport:   opts.Transport,
		doorbell:    opts.Doorbell,
		giveUpAfter: opts.GiveUpAfter,
		retryBase:   opts.RetryBase,
		retryMax:    opts.RetryMax,
		wake:        make(chan struct{}, 1),
		outboxWake:  make(chan struct{}, 1),
	}
	if err := m.recover(); err != nil {
		db.Close()
		return nil, err
	}
	return m, nil
}

// Close closes the database. Call it after Run has returned.
func (m *Mailbox) Close() error { return m.db.Close() }

// Node is the name of the node this mailbox serves.
func (m *Mailbox) Node() string { return m.node }

// agentFor returns the adapter for name (nil = inbox only) and whether name
// is an agent here at all. baf's own name never is.
func (m *Mailbox) agentFor(name string) (agent.Adapter, bool) {
	if name == SystemAgent {
		return nil, false
	}
	if a, ok := m.agents[name]; ok {
		return a, true
	}
	_, wildcard := m.agents[AnyAgent]
	return nil, wildcard
}

// Send stores a message from one of this node's own agents. It fills in the
// version and id if they are empty and returns the receiver's receipt.
func (m *Mailbox) Send(env envelope.Envelope) (Receipt, error) {
	if env.V == 0 {
		env.V = envelope.Version
	}
	if env.ID == "" {
		env.ID = envelope.NewID()
	}
	if _, ok := m.agentFor(env.FromAgent); !ok {
		return Receipt{}, reject("%q is not an agent on %s", env.FromAgent, m.node)
	}
	// A typo in the machine name fails now, not after 24 hours of retries.
	if to, err := envelope.ParseAddress(env.To); err == nil && to.Node != m.node && m.transport != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := m.transport.Known(ctx, to.Node)
		cancel()
		if err != nil {
			return Receipt{}, &Rejection{Reason: err.Error()}
		}
	}
	var r Receipt
	err := m.inTx(func(tx *sql.Tx) (err error) {
		r, err = m.sendTx(tx, env)
		return err
	})
	if err == nil {
		m.signal()
	}
	return r, err
}

// Accept stores a message that arrived from principal. The caller has
// established principal; nothing in env is trusted for it.
func (m *Mailbox) Accept(env envelope.Envelope, from Principal) (Receipt, error) {
	var r Receipt
	err := m.inTx(func(tx *sql.Tx) (err error) {
		r, err = m.acceptTx(tx, env, from)
		return err
	})
	if err == nil {
		m.signal()
	}
	return r, err
}

// sendTx routes env from a local agent: straight into the receiver's inbox
// on this node, or into the outbox for another node.
func (m *Mailbox) sendTx(tx *sql.Tx, env envelope.Envelope) (Receipt, error) {
	if err := env.Validate(); err != nil {
		return Receipt{}, &Rejection{Reason: err.Error()}
	}
	to, _ := envelope.ParseAddress(env.To) // checked by Validate
	var (
		r   Receipt
		err error
	)
	switch {
	case to.Node == m.node:
		r, err = m.acceptTx(tx, env, Principal{Node: m.node, Agent: env.FromAgent})
	case m.transport == nil:
		err = ErrRemoteNotSupported
	default:
		r, err = m.enqueueTx(tx, env, to)
	}
	if err != nil {
		return Receipt{}, err
	}
	// The sender has now given its thread id to the receiver, so the
	// receiver may continue that thread from here on.
	if env.FromThread != "" {
		if err := m.shareTx(tx, env.FromAgent, to.String(), env.FromThread); err != nil {
			return Receipt{}, err
		}
	}
	return r, nil
}

func (m *Mailbox) acceptTx(tx *sql.Tx, env envelope.Envelope, from Principal) (Receipt, error) {
	return m.storeTx(tx, env, from, true)
}

// storeTx stores env for one of this node's agents. checkThread is false
// only for baf's own notices, which may continue any of the agent's threads.
func (m *Mailbox) storeTx(tx *sql.Tx, env envelope.Envelope, from Principal, checkThread bool) (Receipt, error) {
	if err := env.Validate(); err != nil {
		return Receipt{}, &Rejection{Reason: err.Error()}
	}
	to, _ := envelope.ParseAddress(env.To)
	if to.Node != m.node {
		return Receipt{}, reject("this mailbox is %s, not %s", m.node, to.Node)
	}
	adapter, ok := m.agentFor(to.Agent)
	if !ok {
		return Receipt{}, reject("there is no agent %q on %s", to.Agent, m.node)
	}

	// A retry of a message already stored gets the same receipt again.
	var storedAt int64
	err := tx.QueryRow(`SELECT received_at FROM messages WHERE sender_node = ? AND id = ?`,
		from.Node, env.ID).Scan(&storedAt)
	if err == nil {
		return Receipt{ID: env.ID, StoredAt: time.UnixMilli(storedAt), Duplicate: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, err
	}

	if env.ToThread != "" && checkThread {
		if err := m.useThreadTx(tx, to.Agent, from.String(), env.ToThread); err != nil {
			return Receipt{}, err
		}
	}

	state := "pending"
	if adapter == nil {
		state = "inbox"
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return Receipt{}, err
	}
	now := m.now()
	_, err = tx.Exec(`INSERT INTO messages
		(sender_node, id, kind, from_agent, to_agent, to_thread, envelope, state, received_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		from.Node, env.ID, env.Kind, env.FromAgent, to.Agent, env.ToThread, raw, state, now.UnixMilli())
	if err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: env.ID, StoredAt: now}, nil
}

// shareTx records that agent gave threadID to peer, or refreshes its clock.
func (m *Mailbox) shareTx(tx *sql.Tx, agentName, peer, threadID string) error {
	_, err := tx.Exec(`INSERT INTO shared_threads (agent, peer, thread_id, last_used) VALUES (?, ?, ?, ?)
		ON CONFLICT (agent, peer, thread_id) DO UPDATE SET last_used = excluded.last_used`,
		agentName, peer, threadID, m.now().UnixMilli())
	return err
}

// useThreadTx allows peer to continue agent's thread only if agent shared it
// with peer within the last ThreadTTL, and resets that clock.
func (m *Mailbox) useThreadTx(tx *sql.Tx, agentName, peer, threadID string) error {
	var last int64
	err := tx.QueryRow(`SELECT last_used FROM shared_threads WHERE agent = ? AND peer = ? AND thread_id = ?`,
		agentName, peer, threadID).Scan(&last)
	if errors.Is(err, sql.ErrNoRows) {
		return reject("%s never shared thread %s with %s; leave out to_thread to start a new thread",
			agentName, threadID, peer)
	}
	if err != nil {
		return err
	}
	if m.now().Sub(time.UnixMilli(last)) > ThreadTTL {
		return reject("thread %s of %s expired after 30 days without use; leave out to_thread to start a new thread",
			threadID, agentName)
	}
	_, err = tx.Exec(`UPDATE shared_threads SET last_used = ? WHERE agent = ? AND peer = ? AND thread_id = ?`,
		m.now().UnixMilli(), agentName, peer, threadID)
	return err
}

// Run hands waiting messages to their agents and pushes outgoing ones until
// ctx is cancelled. Then it waits until every run it started has been
// stopped and recorded.
func (m *Mailbox) Run(ctx context.Context) {
	var wg sync.WaitGroup
	defer wg.Wait()
	if m.transport != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.runOutbox(ctx)
		}()
	}
	for {
		claims, err := m.claim()
		if err != nil {
			slog.Error("claim messages", "err", err)
		}
		for _, c := range claims {
			wg.Add(1)
			go func() {
				defer wg.Done()
				m.execute(ctx, c)
			}()
		}
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		}
	}
}

// signal tells Run there may be something new to claim or push.
func (m *Mailbox) signal() {
	for _, ch := range []chan struct{}{m.wake, m.outboxWake} {
		select {
		case ch <- struct{}{}:
		default: // a wake-up is already pending
		}
	}
}

// claimed is a message that has been given a run record and may now start.
type claimed struct {
	runID  int64
	agent  string
	env    envelope.Envelope
	sender Principal
}

// claim marks every message that can start now as running and records its
// run, in one transaction, before any agent process starts. A message waits
// while another run holds the thread it wants to continue.
func (m *Mailbox) claim() ([]claimed, error) {
	var out []claimed
	err := m.inTx(func(tx *sql.Tx) error {
		out = nil
		rows, err := tx.Query(`SELECT seq, sender_node, to_agent, to_thread, envelope
			FROM messages WHERE state = 'pending' ORDER BY seq`)
		if err != nil {
			return err
		}
		type pending struct {
			seq                     int64
			node, agentName, thread string
			raw                     []byte
		}
		var list []pending
		for rows.Next() {
			var p pending
			if err := rows.Scan(&p.seq, &p.node, &p.agentName, &p.thread, &p.raw); err != nil {
				rows.Close()
				return err
			}
			list = append(list, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		busy := map[string]bool{} // agent + "\x00" + thread, claimed in this pass
		for _, p := range list {
			if p.thread != "" {
				key := p.agentName + "\x00" + p.thread
				if busy[key] {
					continue
				}
				var n int
				err := tx.QueryRow(`SELECT COUNT(*) FROM runs WHERE state = 'running' AND agent = ? AND thread_id = ?`,
					p.agentName, p.thread).Scan(&n)
				if err != nil {
					return err
				}
				if n > 0 {
					continue // the thread is in use; try again when that run ends
				}
				busy[key] = true
			}
			var env envelope.Envelope
			if err := json.Unmarshal(p.raw, &env); err != nil {
				return fmt.Errorf("message %d: %w", p.seq, err)
			}
			// The agent gets the whole message in its run, so it counts as read.
			if _, err := tx.Exec(`UPDATE messages SET state = 'running', read_at = COALESCE(read_at, ?) WHERE seq = ?`,
				m.now().UnixMilli(), p.seq); err != nil {
				return err
			}
			res, err := tx.Exec(`INSERT INTO runs (message_seq, agent, thread_id, state, started_at)
				VALUES (?, ?, ?, 'running', ?)`, p.seq, p.agentName, p.thread, m.now().UnixMilli())
			if err != nil {
				return err
			}
			runID, _ := res.LastInsertId()
			out = append(out, claimed{
				runID:  runID,
				agent:  p.agentName,
				env:    env,
				sender: Principal{Node: p.node, Agent: env.FromAgent},
			})
		}
		return nil
	})
	return out, err
}

// execute runs one claimed message through its agent's adapter and records
// how it ended.
func (m *Mailbox) execute(ctx context.Context, c claimed) {
	job := agent.Job{
		Message:      c.env,
		Sender:       c.sender,
		ResumeThread: c.env.ToThread,
		Env:          m.runEnv(c.agent),
	}
	slog.Info("run started", "run", c.runID, "agent", c.agent, "message", c.env.ID, "from", c.sender)
	adapter, _ := m.agentFor(c.agent) // claim only takes messages for agents with an adapter
	out := adapter.Run(ctx, job, func(threadID string) {
		// Record the thread as soon as the agent reports it, so a crash
		// later still leaves the thread locked to this run's record.
		if _, err := m.db.Exec(`UPDATE runs SET thread_id = ? WHERE id = ?`, threadID, c.runID); err != nil {
			slog.Error("record thread id", "run", c.runID, "err", err)
		}
	})
	if ctx.Err() != nil {
		out.State = envelope.Failed
		out.Text = "Stopped because baf was shutting down or restarting; the work may be partly done.\n" + out.Text
	}
	if err := m.finish(c, out); err != nil {
		slog.Error("record run end", "run", c.runID, "err", err)
	}
	slog.Info("run ended", "run", c.runID, "agent", c.agent, "state", out.State)
	m.signal() // the thread is free again
}

// finish records a run's end and, for a request, queues its Result, in one
// transaction, so a crash can't record one without the other.
func (m *Mailbox) finish(c claimed, out agent.Outcome) error {
	err := m.inTx(func(tx *sql.Tx) error {
		if out.ThreadID != "" {
			if _, err := tx.Exec(`UPDATE runs SET thread_id = ? WHERE id = ?`, out.ThreadID, c.runID); err != nil {
				return err
			}
		}
		var threadID string
		if err := tx.QueryRow(`SELECT thread_id FROM runs WHERE id = ?`, c.runID).Scan(&threadID); err != nil {
			return err
		}
		return m.endRunTx(tx, c.runID, c.agent, threadID, c.env, c.sender, out.State, out.Text)
	})
	return err
}

// endRunTx closes a run, marks its message done and, if the message was a
// request, sends the automatic Result back to the sender.
func (m *Mailbox) endRunTx(tx *sql.Tx, runID int64, agentName, threadID string, req envelope.Envelope,
	sender Principal, state envelope.ResultState, text string) error {
	_, err := tx.Exec(`UPDATE runs SET state = ?, detail = ?, ended_at = ? WHERE id = ?`,
		state, truncate(text, 4000), m.now().UnixMilli(), runID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE messages SET state = 'done' WHERE seq = (SELECT message_seq FROM runs WHERE id = ?)`, runID)
	if err != nil {
		return err
	}
	if req.Kind != envelope.Request {
		return nil // notices and results are never answered automatically
	}
	result := envelope.Envelope{
		V:          envelope.Version,
		ID:         envelope.NewID(),
		Kind:       envelope.Result,
		To:         sender.String(),
		FromAgent:  agentName,
		FromThread: threadID,
		ToThread:   req.FromThread, // a reply swaps the two thread ids
		ReplyTo:    req.ID,
		Body:       envelope.Body{Text: text, State: state},
	}
	if _, err := m.sendTx(tx, result); err != nil {
		// The run's end is still recorded; only the answer is lost. This
		// happens only if the sender's agent no longer exists here.
		slog.Error("send result", "request", req.ID, "to", sender, "err", err)
	}
	return nil
}

// recover settles runs a crash left open: the mailbox knows only that they
// started, so each becomes "unknown" and is never run again (design notes
// section 7). It also forgets shared threads that have expired.
func (m *Mailbox) recover() error {
	return m.inTx(func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT r.id, r.agent, r.thread_id, m.sender_node, m.envelope
			FROM runs r JOIN messages m ON m.seq = r.message_seq WHERE r.state = 'running'`)
		if err != nil {
			return err
		}
		type open struct {
			runID                   int64
			agentName, thread, node string
			raw                     []byte
		}
		var list []open
		for rows.Next() {
			var o open
			if err := rows.Scan(&o.runID, &o.agentName, &o.thread, &o.node, &o.raw); err != nil {
				rows.Close()
				return err
			}
			list = append(list, o)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, o := range list {
			var env envelope.Envelope
			if err := json.Unmarshal(o.raw, &env); err != nil {
				return err
			}
			slog.Warn("run outcome unknown after a crash", "run", o.runID, "agent", o.agentName, "message", env.ID)
			err := m.endRunTx(tx, o.runID, o.agentName, o.thread, env, Principal{Node: o.node, Agent: env.FromAgent},
				envelope.Unknown,
				"baf lost track of this run (it crashed or was killed while the agent was working). "+
					"The work may or may not have finished; check before asking again.")
			if err != nil {
				return err
			}
		}
		cutoff := m.now().Add(-ThreadTTL).UnixMilli()
		_, err = tx.Exec(`DELETE FROM shared_threads WHERE last_used < ?`, cutoff)
		return err
	})
}

func (m *Mailbox) inTx(fn func(*sql.Tx) error) error {
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
