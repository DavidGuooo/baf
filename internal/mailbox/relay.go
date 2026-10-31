package mailbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/DavidGuooo/baf/internal/tailnet"
)

// Relay discovery is optional for transports that only deliver directly.
type relayFinder interface {
	Relay(context.Context, bool) (string, error)
}

func (m *Mailbox) relay(ctx context.Context, refresh bool) (string, error) {
	finder, ok := m.transport.(relayFinder)
	if !ok {
		return "", reject("no Relay found: this transport cannot find the Relay")
	}
	node, err := finder.Relay(ctx, refresh)
	if err != nil {
		return "", err
	}
	if node == m.node {
		return "", reject("the Relay doesn't hold this Door agent")
	}
	return node, nil
}

// route keeps the envelope's address, but chooses the node holding its mail.
func (m *Mailbox) route(to envelope.Address) (string, error) {
	if to.Node == m.node {
		return m.node, nil
	}
	door, err := m.doorDestination(to)
	if err != nil {
		return "", err
	}
	if door {
		return m.node, nil
	}
	if m.transport == nil {
		return "", ErrRemoteNotSupported
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.transport.Known(ctx, to.Node); err == nil {
		return to.Node, nil
	} else if !errors.Is(err, tailnet.ErrNoSuchNode) {
		return "", err
	}
	if m.door != nil {
		peer, err := m.doorPeer()
		if err != nil {
			return "", err
		}
		if peer.IsRelay() {
			return "", reject("the Relay doesn't hold %s: the Tailscale policy does not list it as a Door agent", to)
		}
	}
	node, err := m.relay(ctx, false)
	if err != nil {
		return "", reject("cannot send to %s: %s", to, err)
	}
	return node, nil
}

// CheckIncoming applies the same policy to a push and its file uploads.
// The caller still takes the agent name from the envelope for a direct push.
func (m *Mailbox) CheckIncoming(ctx context.Context, peer tailnet.Peer, to envelope.Address, origin string) (Principal, error) {
	target := to.Agent
	if to.Node != m.node {
		names, err := m.DoorAgents()
		if err != nil {
			return Principal{}, err
		}
		if !slices.Contains(names, to.String()) {
			return Principal{}, reject("the Relay doesn't hold %s", to)
		}
		target = to.String()
	}
	if origin == "" {
		if !peer.MayMessage(target) {
			return Principal{}, reject("%s may not message %s: no %s grant with to naming %q in the Tailscale policy", peer.Node, to, tailnet.Capability, target)
		}
		return Principal{Node: peer.Node}, nil
	}
	if !peer.IsRelay() {
		return Principal{}, reject("%s may not set origin: it has no relay: true grant", peer.Node)
	}
	from, err := envelope.ParseAddress(origin)
	if err != nil {
		return Principal{}, reject("bad origin: %s", err)
	}
	if from.Node == m.node || from.Node == peer.Node {
		return Principal{}, reject("origin %s names a tailnet machine; the Relay vouches only for Door agents", origin)
	}
	if m.transport == nil {
		return Principal{}, fmt.Errorf("cannot verify origin's vendor against the tailnet")
	}
	if err := m.transport.Known(ctx, from.Node); err == nil {
		return Principal{}, reject("origin %s names a tailnet machine; the Relay vouches only for Door agents", origin)
	} else if !errors.Is(err, tailnet.ErrNoSuchNode) {
		return Principal{}, err
	}
	if !slices.Contains(peer.DoorAgents(), origin) {
		return Principal{}, reject("origin %s is not named in %s's relay grants", origin, peer.Node)
	}
	if !peer.DoorMayMessage(origin, target) {
		return Principal{}, reject("%s may not message %s: its door entry in the Tailscale policy does not allow %q", origin, to, target)
	}
	return from, nil
}
