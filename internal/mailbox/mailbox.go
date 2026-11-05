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
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/DavidGuooo/baf/internal/agent"
	"github.com/DavidGuooo/baf/internal/envelope"
)

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

	// FilesDir holds the bytes of files sent and received (ADR-0007).
	// Default: "files" next to Path.
	FilesDir string

	// Agents served by this mailbox. A nil adapter means the agent only has
	// an inbox (for example "me", the owner at a terminal). The key AnyAgent
	// (with a nil adapter) gives every other name an inbox.
	Agents map[string]agent.Adapter

	// RunEnv returns extra environment for an agent's run. Optional.
	RunEnv func(agentName string) []string

	// NewThreadDir chooses a resolved folder when a new thread has no run.dir.
	// Nil, or an empty result, keeps the adapter's default.
	NewThreadDir func(agentName string) (string, error)
	UserHome     string // receiver's home; defaults to os.UserHomeDir
	BafBinary    string
	BafHome      string // nonempty when hand-off commands need BAF_HOME

	// Transport pushes messages to other nodes. Without one, the mailbox
	// only delivers between its own agents.
	Transport Transport

	// Doorbell wakes another node's agents. Optional.
	Doorbell Doorbell

	// Door reads this node's grants. Nil means the Public door is disabled.
	Door DoorIdentity

	GiveUpAfter time.Duration // default DefaultGiveUpAfter
	RetryDelay  time.Duration // wait after a failed try before the next; default 5s

	Now func() time.Time // for tests; defaults to time.Now
}

// Mailbox is safe for concurrent use.
type Mailbox struct {
	db           *sql.DB
	node         string
	filesDir     string
	filesMu      sync.Mutex
	agents       map[string]agent.Adapter
	runEnv       func(string) []string
	newThreadDir func(string) (string, error)
	userHome     string
	bafBinary    string
	bafHome      string
	now          func() time.Time
	transport    Transport
	doorbell     Doorbell
	door         DoorIdentity
	eventsMu     sync.RWMutex
	events       DoorEvents
	giveUpAfter  time.Duration
	retryDelay   time.Duration

	wake       chan struct{} // nudges Run to look for claimable messages
	outboxWake chan struct{} // nudges the outbox to look for messages to push
}

// Open opens the mailbox and settles anything a previous crash left behind.
func Open(opts Options) (*Mailbox, error) {
	if opts.UserHome == "" {
		var err error
		opts.UserHome, err = os.UserHomeDir()
		if err != nil {
			return nil, err
		}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.RunEnv == nil {
		opts.RunEnv = func(string) []string { return nil }
	}
	if opts.GiveUpAfter <= 0 {
		opts.GiveUpAfter = DefaultGiveUpAfter
	}
	if opts.RetryDelay <= 0 {
		opts.RetryDelay = 5 * time.Second
	}
	if _, ok := opts.Agents[SystemAgent]; ok {
		return nil, fmt.Errorf("%q is reserved for baf's own notices; rename that agent", SystemAgent)
	}
	if a, ok := opts.Agents[AnyAgent]; ok && a != nil {
		return nil, fmt.Errorf("%q can only give inboxes; it cannot run an agent", AnyAgent)
	}
	if opts.FilesDir == "" {
		opts.FilesDir = filepath.Join(filepath.Dir(opts.Path), "files")
	}
	filesDir, err := filepath.Abs(opts.FilesDir)
	if err != nil {
		return nil, err
	}
	opts.FilesDir = filesDir
	if _, err := envelope.ParseAddress(opts.Node + "/x"); err != nil {
		return nil, fmt.Errorf("bad node name %q: %w", opts.Node, err)
	}
	db, err := openDB(opts.Path)
	if err != nil {
		return nil, err
	}
	m := &Mailbox{
		db:           db,
		node:         opts.Node,
		filesDir:     opts.FilesDir,
		agents:       opts.Agents,
		runEnv:       opts.RunEnv,
		newThreadDir: opts.NewThreadDir,
		userHome:     opts.UserHome,
		bafBinary:    opts.BafBinary,
		bafHome:      opts.BafHome,
		now:          opts.Now,
		transport:    opts.Transport,
		doorbell:     opts.Doorbell,
		door:         opts.Door,
		giveUpAfter:  opts.GiveUpAfter,
		retryDelay:   opts.RetryDelay,
		wake:         make(chan struct{}, 1),
		outboxWake:   make(chan struct{}, 1),
	}
	if err := m.recover(); err != nil {
		db.Close()
		return nil, err
	}
	if err := m.expireAnswers(); err != nil {
		slog.Error("expire unanswered requests", "err", err)
	}
	if err := m.cleanFiles(); err != nil {
		slog.Error("clean files", "err", err)
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
	if _, err := envelope.ParseAddress("x/" + name); err != nil {
		return nil, false
	}
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
	env.Origin = "" // Local agents cannot vouch for a Door agent.
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
		r, err = m.storeTx(tx, env, from, "")
		return err
	})
	if err == nil {
		m.signal()
	}
	return r, err
}

// AcceptForwarded records the connecting Relay alongside the trusted Principal.
// Only that Relay may later ask about this forwarded message's progress.
func (m *Mailbox) AcceptForwarded(env envelope.Envelope, from Principal, relay string) (Receipt, error) {
	var r Receipt
	err := m.inTx(func(tx *sql.Tx) (err error) {
		r, err = m.storeTx(tx, env, from, "")
		if err == nil && !r.Duplicate {
			_, err = tx.Exec(`UPDATE messages SET forwarding_node = ? WHERE sender_node = ? AND id = ?`, relay, from.Node, env.ID)
		}
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
	if err := m.checkBlobs(env.Body.Files, true); err != nil {
		return Receipt{}, err
	}
	to, _ := envelope.ParseAddress(env.To) // checked by Validate
	node, err := m.route(to)
	if err != nil {
		return Receipt{}, err
	}
	switch {
	case to.Node == m.node:
		return m.storeTx(tx, env, Principal{Node: m.node, Agent: env.FromAgent}, "")
	case node == m.node:
		return m.storeInboxTx(tx, env, Principal{Node: m.node, Agent: env.FromAgent}, to.String(), nil, "")
	default:
		return m.enqueueTx(tx, env, envelope.Address{Node: node, Agent: to.Agent})
	}
}

// storeTx stores env for one of this node's agents. Any sender may continue
// any of the agent's threads with to_thread; the policy decides who may
// message the agent at all.
// dir is run.dir already resolved and checked against the sender's grants
// (AcceptPush); empty means it still needs checking, or there is none.
func (m *Mailbox) storeTx(tx *sql.Tx, env envelope.Envelope, from Principal, dir string) (Receipt, error) {
	env.FromAgent = from.Agent
	if err := env.Validate(); err != nil {
		return Receipt{}, &Rejection{Reason: err.Error()}
	}
	to, _ := envelope.ParseAddress(env.To)
	if to.Node != m.node {
		if _, _, err := m.requireDoor(to.String()); err != nil {
			return Receipt{}, err
		}
		return m.storeInboxTx(tx, env, from, to.String(), nil, dir)
	}
	adapter, ok := m.agentFor(to.Agent)
	if !ok {
		return Receipt{}, reject("there is no agent %q on %s", to.Agent, m.node)
	}
	return m.storeInboxTx(tx, env, from, to.Agent, adapter, dir)
}

// storeInboxTx uses the full address as the inbox name for a Door agent.
// Its nil adapter means mail waits here until the Door agent reads it.
func (m *Mailbox) storeInboxTx(tx *sql.Tx, env envelope.Envelope, from Principal, inbox string, adapter agent.Adapter, dir string) (Receipt, error) {
	// A retry of a message already stored gets the same receipt again.
	var storedAt int64
	var fromAgent string
	err := tx.QueryRow(`SELECT received_at, from_agent FROM messages WHERE sender_node = ? AND id = ?`,
		from.Node, env.ID).Scan(&storedAt, &fromAgent)
	if err == nil {
		if fromAgent != from.Agent {
			return Receipt{}, reject("message id %q is already used by another agent; choose a new id", env.ID)
		}
		return Receipt{ID: env.ID, StoredAt: time.UnixMilli(storedAt), Duplicate: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, err
	}
	dir, err = m.checkRun(tx, env, from, inbox, adapter, dir)
	if err != nil {
		return Receipt{}, err
	}

	if err := m.placeFiles(from.Node, env.ID, env.Body.Files); err != nil {
		return Receipt{}, err
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
		(sender_node, id, kind, from_agent, to_agent, to_thread, envelope, state, received_at, run_dir)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		from.Node, env.ID, env.Kind, env.FromAgent, inbox, env.ToThread, raw, state, now.UnixMilli(), dir)
	if err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: env.ID, StoredAt: now}, nil
}

// Run hands waiting messages to their agents and pushes outgoing ones until
// ctx is cancelled. Then it waits until every run it started has been
// stopped and recorded.
func (m *Mailbox) Run(ctx context.Context) {
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		answers := time.NewTicker(time.Minute)
		defer answers.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-answers.C:
				if err := m.expireAnswers(); err != nil {
					slog.Error("expire unanswered requests", "err", err)
				}
			case <-ticker.C:
				if err := m.cleanFiles(); err != nil {
					slog.Error("clean files", "err", err)
				}
			}
		}
	}()
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
	m.eventsMu.RLock()
	if m.events != nil {
		m.events.Wake()
	}
	m.eventsMu.RUnlock()
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
	dir    string
}

// claim marks every message that can start now as running and records its
// run, in one transaction, before any agent process starts. A message waits
// while another run holds the thread it wants to continue.
func (m *Mailbox) claim() ([]claimed, error) {
	var out []claimed
	err := m.inTx(func(tx *sql.Tx) error {
		out = nil
		rows, err := tx.Query(`SELECT seq, sender_node, to_agent, to_thread, envelope, run_dir
			FROM messages WHERE state = 'pending' ORDER BY seq`)
		if err != nil {
			return err
		}
		type pending struct {
			seq                     int64
			node, agentName, thread string
			raw                     []byte
			dir                     string
		}
		var list []pending
		for rows.Next() {
			var p pending
			if err := rows.Scan(&p.seq, &p.node, &p.agentName, &p.thread, &p.raw, &p.dir); err != nil {
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
			desktop := env.Run != nil && env.Run.Desktop
			res, err := tx.Exec(`INSERT INTO runs (message_seq, agent, thread_id, state, started_at, desktop)
				VALUES (?, ?, ?, 'running', ?, ?)`, p.seq, p.agentName, p.thread, m.now().UnixMilli(), desktop)
			if err != nil {
				return err
			}
			runID, _ := res.LastInsertId()
			out = append(out, claimed{
				runID:  runID,
				agent:  p.agentName,
				env:    env,
				sender: Principal{Node: p.node, Agent: env.FromAgent},
				dir:    p.dir,
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
		BafBinary:    m.bafBinary,
		BafHome:      m.bafHome,
	}
	desktop := c.env.Run != nil && c.env.Run.Desktop
	if desktop {
		job.Env = agent.WithoutIdentity(job.Env)
	}
	dir, err := m.runDir(c)
	if err != nil {
		if err := m.finish(c, agent.Outcome{State: envelope.Failed, Text: "prepare run folder: " + err.Error()}); err != nil {
			slog.Error("record run end", "run", c.runID, "err", err)
		}
		m.signal()
		return
	}
	job.Dir = dir
	for _, f := range c.env.Body.Files {
		job.Files = append(job.Files, agent.File{Name: f.Name, Size: f.Size, Path: m.filePath(c.sender.Node, c.env.ID, f.Name)})
	}
	slog.Info("run started", "run", c.runID, "agent", c.agent, "message", c.env.ID, "from", c.sender)
	adapter, _ := m.agentFor(c.agent) // claim only takes messages for agents with an adapter
	out := adapter.Run(ctx, job, agent.Report{
		Thread: func(threadID string) {
			// Record the thread as soon as the agent reports it, so a crash
			// later still leaves the thread locked to this run's record.
			if _, err := m.db.Exec(`UPDATE runs SET thread_id = ? WHERE id = ?`, threadID, c.runID); err != nil {
				slog.Error("record thread id", "run", c.runID, "err", err)
			}
		},
		Doing: func(text string) {
			// Only for whoever asks (Progress); nothing is sent anywhere.
			_, err := m.db.Exec(`UPDATE runs SET doing = ?, doing_at = ? WHERE id = ? AND doing <> ?`,
				text, m.now().UnixMilli(), c.runID, text)
			if err != nil {
				slog.Error("record what the agent is doing", "run", c.runID, "err", err)
			}
		},
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
	if desktop {
		var thread string
		if err := m.db.QueryRow(`SELECT thread_id FROM runs WHERE id = ?`, c.runID).Scan(&thread); err != nil {
			slog.Warn("read Desktop thread", "run", c.runID, "err", err)
		} else if opener, ok := adapter.(agent.DesktopOpener); ok && thread != "" {
			// App startup must not delay recording or dispatching another run.
			go func() {
				if err := opener.OpenDesktop(thread); err != nil {
					slog.Warn("open Claude Desktop", "run", c.runID, "err", err)
				}
			}()
		}
	}
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
		// can happen if the sender's agent no longer exists here, or its
		// Door policy can no longer be read or no longer names it.
		slog.Error("send result", "request", req.ID, "to", sender, "err", err)
	}
	return nil
}

// recover settles runs a crash left open: the mailbox knows only that they
// started, so each becomes "unknown" and is never run again.
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
		return nil
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
