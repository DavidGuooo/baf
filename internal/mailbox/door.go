package mailbox

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/DavidGuooo/baf/internal/tailnet"
)

// DoorIdentity reads this node's own grants from Tailscale. Tests use a fake.
// A successful answer must reflect the current policy, or a short cache of it.
type DoorIdentity interface {
	SelfPeer(context.Context) (tailnet.Peer, error)
}

func (m *Mailbox) doorPeer() (tailnet.Peer, error) {
	if m.door == nil {
		return tailnet.Peer{}, reject("the Public door is disabled on %s", m.node)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	peer, err := m.door.SelfPeer(ctx)
	if err != nil {
		return tailnet.Peer{}, fmt.Errorf("reading this node's Door agents from Tailscale: %w", err)
	}
	return peer, nil
}

// DoorAgents names the Door agents this mailbox currently holds. The Public
// door also uses this to check a name before offering sign-in.
func (m *Mailbox) DoorAgents() ([]string, error) {
	peer, err := m.doorPeer()
	if err != nil {
		return nil, err
	}
	return peer.DoorAgents(), nil
}

func (m *Mailbox) requireDoor(name string) (Principal, tailnet.Peer, error) {
	from, err := envelope.ParseAddress(name)
	if err != nil {
		return Principal{}, tailnet.Peer{}, reject("bad Door agent name: %s", err)
	}
	peer, err := m.doorPeer()
	if err != nil {
		return Principal{}, tailnet.Peer{}, err
	}
	if !slices.Contains(peer.DoorAgents(), name) {
		return Principal{}, tailnet.Peer{}, reject("the Tailscale policy does not list %q as a Door agent on this Relay", name)
	}
	return from, peer, nil
}

// doorDestination reports whether this mailbox holds the destination.
// route resolves other names to a tailnet machine or the Relay.
func (m *Mailbox) doorDestination(to envelope.Address) (bool, error) {
	if to.Node == m.node || m.door == nil {
		return false, nil
	}
	names, err := m.DoorAgents()
	if err != nil {
		return false, err
	}
	if slices.Contains(names, to.String()) {
		return true, nil
	}
	return false, nil
}

// FromDoor stores mail from an authenticated Door agent. The Public door
// supplies doorAgent from its token, never from fields in the message.
// Authenticated file uploads from Door agents are not supported yet.
func (m *Mailbox) FromDoor(doorAgent string, env envelope.Envelope) (Receipt, error) {
	from, peer, err := m.requireDoor(doorAgent)
	if err != nil {
		return Receipt{}, err
	}
	env.FromAgent, env.Origin = from.Agent, ""
	if env.V == 0 {
		env.V = envelope.Version
	}
	if env.ID == "" {
		env.ID = envelope.NewID()
	}
	if err := env.Validate(); err != nil {
		return Receipt{}, &Rejection{Reason: err.Error()}
	}
	to, _ := envelope.ParseAddress(env.To)
	node, err := m.route(to)
	if err != nil {
		return Receipt{}, err
	}
	target := to.Agent
	if node == m.node && to.Node != m.node {
		target = to.String()
	}
	if node == m.node && !peer.DoorMayMessage(doorAgent, target) {
		return Receipt{}, reject("%s may not message %s: its door entry in the Tailscale policy does not allow %q", doorAgent, env.To, target)
	}
	if node != m.node {
		env.Origin = doorAgent
	}
	if len(env.Body.Files) != 0 {
		return Receipt{}, reject("files from Door agents are not supported yet")
	}
	var r Receipt
	err = m.inTx(func(tx *sql.Tx) (err error) {
		if node == m.node {
			r, err = m.storeTx(tx, env, from)
		} else {
			r, err = m.enqueueTx(tx, env, envelope.Address{Node: node, Agent: to.Agent})
		}
		return err
	})
	if err == nil {
		m.signal()
	}
	return r, err
}

// DoorInbox lists only this Door agent's mail. Listing never marks it read.
func (m *Mailbox) DoorInbox(doorAgent string, unread bool, limit int) ([]Item, error) {
	if _, _, err := m.requireDoor(doorAgent); err != nil {
		return nil, err
	}
	return m.Inbox(doorAgent, unread, limit)
}

// DoorRead opens one message and marks it read, without confirming or
// deleting it. It cannot open another agent's mail.
func (m *Mailbox) DoorRead(doorAgent, id string) (Item, error) {
	if _, _, err := m.requireDoor(doorAgent); err != nil {
		return Item{}, err
	}
	return m.Read(doorAgent, id)
}

// DoorStatus reports only on mail this Door agent sent, including any Result.
func (m *Mailbox) DoorStatus(ctx context.Context, doorAgent, id string) (SentStatus, error) {
	from, _, err := m.requireDoor(doorAgent)
	if err != nil {
		return SentStatus{}, err
	}
	return m.sentStatus(ctx, from, id)
}
