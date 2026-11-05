package mailbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/DavidGuooo/baf/internal/agent"
	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/DavidGuooo/baf/internal/runfolder"
	"github.com/DavidGuooo/baf/internal/tailnet"
)

// AcceptPush checks the caller's grants and stores the exact folder checked.
func (m *Mailbox) AcceptPush(ctx context.Context, env envelope.Envelope, peer tailnet.Peer) (Receipt, error) {
	if err := env.Validate(); err != nil {
		return Receipt{}, reject("%s", err)
	}
	if env.Origin != "" && env.Run != nil {
		return Receipt{}, reject("messages with origin may not carry run options")
	}
	to, _ := envelope.ParseAddress(env.To)
	from, err := m.CheckIncoming(ctx, peer, to, env.Origin)
	if err != nil {
		return Receipt{}, err
	}
	if env.Origin == "" {
		from.Agent = env.FromAgent
	}
	var dir string
	if env.Run != nil && env.Run.Dir != "" {
		dir, err = runfolder.Resolve(env.Run.Dir, m.userHome)
		if err != nil {
			return Receipt{}, reject("run.dir: %s", err)
		}
		if !peer.MayRunIn(dir, m.userHome) {
			return Receipt{}, reject("%s may not use run.dir %q: no dirs field in its Tailscale grants allows this folder", peer.Node, env.Run.Dir)
		}
	}
	var receipt Receipt
	err = m.inTx(func(tx *sql.Tx) (err error) {
		receipt, err = m.storeTx(tx, env, from, dir)
		if err == nil && !receipt.Duplicate && env.Origin != "" {
			_, err = tx.Exec(`UPDATE messages SET forwarding_node = ? WHERE sender_node = ? AND id = ?`, peer.Node, from.Node, env.ID)
		}
		return err
	})
	if err == nil {
		m.signal()
	}
	return receipt, err
}

func (m *Mailbox) checkRun(tx *sql.Tx, env envelope.Envelope, from Principal, inbox string, adapter agent.Adapter, dir string) (string, error) {
	if env.Run != nil {
		if env.Origin != "" {
			return "", reject("messages with origin may not carry run options")
		}
		if env.Run.Desktop {
			if _, ok := adapter.(agent.DesktopOpener); !ok {
				return "", reject("only Claude agents can be handed to the Desktop app")
			}
		}
		if env.Run.Dir != "" && dir == "" {
			if from.Node != m.node {
				return "", reject("run.dir from %s requires a checked dirs grant", from.Node)
			}
			var err error
			dir, err = runfolder.Resolve(env.Run.Dir, m.userHome)
			if err != nil {
				return "", reject("run.dir: %s", err)
			}
		}
	}
	if env.ToThread != "" {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM runs WHERE agent = ? AND thread_id = ? AND desktop = 1`, inbox, env.ToThread).Scan(&n); err != nil {
			return "", err
		}
		if n > 0 {
			return "", reject("thread %s was handed to the Claude Desktop app; leave out to_thread to start a new thread", env.ToThread)
		}
	}
	return dir, nil
}

func (m *Mailbox) runDir(c claimed) (string, error) {
	dir := c.dir
	if c.env.ToThread != "" {
		err := m.db.QueryRow(`SELECT dir FROM runs WHERE agent = ? AND thread_id = ? AND id < ? ORDER BY id DESC LIMIT 1`,
			c.agent, c.env.ToThread, c.runID).Scan(&dir)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	} else if dir == "" && m.newThreadDir != nil {
		var err error
		dir, err = m.newThreadDir(c.agent)
		if err != nil {
			return "", err
		}
	}
	if _, err := m.db.Exec(`UPDATE runs SET dir = ? WHERE id = ?`, dir, c.runID); err != nil {
		return "", err
	}
	if dir != "" {
		// Refuse a stored path whose prefix was replaced with a symlink while
		// the message waited. Never switch to a different folder at run time.
		resolved, err := runfolder.Resolve(dir, m.userHome)
		if err != nil {
			return "", err
		}
		if resolved != dir {
			return "", fmt.Errorf("run folder %q changed since it was chosen", dir)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	return dir, nil
}
