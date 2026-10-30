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
	ended_at    INTEGER
);
CREATE INDEX IF NOT EXISTS runs_by_state ON runs (state, agent, thread_id);

-- The "shared with" list (design notes section 6): agent gave thread_id to
-- peer, so peer may continue that thread. Expires 30 days after last use.
CREATE TABLE IF NOT EXISTS shared_threads (
	agent     TEXT NOT NULL,
	peer      TEXT NOT NULL,    -- node/agent
	thread_id TEXT NOT NULL,
	last_used INTEGER NOT NULL,
	PRIMARY KEY (agent, peer, thread_id)
);
`

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
	return db, nil
}
