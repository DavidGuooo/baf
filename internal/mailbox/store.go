package mailbox

import (
	"database/sql"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, so baf stays one file with no C toolchain
)

// schema is applied on every start; each statement must be safe to repeat.
const schema = `
-- Every message this mailbox has stored for one of its agents.
CREATE TABLE IF NOT EXISTS messages (
	seq         INTEGER PRIMARY KEY AUTOINCREMENT,
	sender_node TEXT NOT NULL,  -- from Tailscale (or this node, for local senders)
	id          TEXT NOT NULL,  -- the sender-chosen message id
	kind        TEXT NOT NULL,
	from_agent  TEXT NOT NULL,
	to_agent    TEXT NOT NULL,
	to_thread   TEXT NOT NULL DEFAULT '',
	envelope    TEXT NOT NULL,  -- the message as JSON, exactly as stored
	state       TEXT NOT NULL,  -- pending | running | done | inbox (agent with no adapter)
	received_at INTEGER NOT NULL, -- unix milliseconds
	read_at     INTEGER,
	UNIQUE (sender_node, id)    -- a retry of the same message is stored once
);
CREATE INDEX IF NOT EXISTS messages_by_agent ON messages (to_agent, state);

-- One row per attempt to hand a message to an agent. Written before the
-- agent's process starts, so a crash can never leave a run unrecorded.
CREATE TABLE IF NOT EXISTS runs (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	message_seq INTEGER NOT NULL REFERENCES messages (seq),
	agent       TEXT NOT NULL,
	thread_id   TEXT NOT NULL DEFAULT '',
	state       TEXT NOT NULL,  -- running | completed | failed | unknown
	detail      TEXT NOT NULL DEFAULT '',
	started_at  INTEGER NOT NULL,
	ended_at    INTEGER,
	doing       TEXT NOT NULL DEFAULT '', -- what the agent last started doing, from its own output
	doing_at    INTEGER
);
CREATE INDEX IF NOT EXISTS runs_by_state ON runs (state, agent, thread_id);

-- The "shared with" list: agent gave thread_id to
-- peer, so peer may continue that thread. Expires 30 days after last use.
CREATE TABLE IF NOT EXISTS shared_threads (
	agent     TEXT NOT NULL,
	peer      TEXT NOT NULL,    -- node/agent
	thread_id TEXT NOT NULL,
	last_used INTEGER NOT NULL,
	PRIMARY KEY (agent, peer, thread_id)
);

-- Messages this node's agents sent to other nodes, held here until the
-- receiver's mailbox gives a receipt (ADR-0002: messages wait at the sender).
CREATE TABLE IF NOT EXISTS outbox (
	seq         INTEGER PRIMARY KEY AUTOINCREMENT,
	id          TEXT NOT NULL UNIQUE,
	to_node     TEXT NOT NULL,
	queue       TEXT NOT NULL,  -- receiver + sender + sender's thread: order is kept per queue
	envelope    TEXT NOT NULL,
	state       TEXT NOT NULL,  -- waiting | handed | failed
	attempts    INTEGER NOT NULL DEFAULT 0,
	rung        INTEGER NOT NULL DEFAULT 0, -- 1 once the receiver's doorbell rang for it
	last_error  TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	next_try_at INTEGER NOT NULL,
	expires_at  INTEGER NOT NULL, -- give up after this (24 hours by default)
	done_at     INTEGER
);
CREATE INDEX IF NOT EXISTS outbox_by_queue ON outbox (state, queue, seq);
`

// addedColumns were added to a table after it was first created. CREATE
// TABLE IF NOT EXISTS leaves an older table as it was, so openDB adds them.
var addedColumns = []struct{ table, column, decl string }{
	{"messages", "notified_at", "INTEGER"},
	{"messages", "forwarding_node", "TEXT NOT NULL DEFAULT ''"},
	{"runs", "doing", "TEXT NOT NULL DEFAULT ''"},
	{"runs", "doing_at", "INTEGER"},
}

// openDB opens (or creates) the SQLite file at path.
func openDB(path string) (*sql.DB, error) {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(FULL)") // a receipt must mean the message is on disk
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	// One connection: every write is serialized, so claims and receipts never
	// race each other. baf's traffic is tiny, so this costs nothing.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	for _, c := range addedColumns {
		var n int
		err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, c.table, c.column).Scan(&n)
		if err == nil && n == 0 {
			_, err = db.Exec("ALTER TABLE " + c.table + " ADD COLUMN " + c.column + " " + c.decl)
		}
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("add column %s.%s: %w", c.table, c.column, err)
		}
	}
	return db, nil
}
