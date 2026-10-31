package tailnet

import (
	"context"
	"fmt"
	"net"
	"slices"
)

// IsRelay reports whether this node may vouch for Door agents here.
func (p Peer) IsRelay() bool {
	for _, g := range p.Grants {
		if g.Relay {
			return true
		}
	}
	return false
}

// Relay finds the one node whose grants toward us name it as the Relay.
// WhoIs reads the local policy, including for our own address.
func (c *Client) Relay(ctx context.Context) (string, error) {
	st, err := c.lc.Status(ctx)
	if err != nil {
		return "", err
	}
	var peers []Peer
	var nodes []Self
	if st.Self != nil {
		nodes = append(nodes, Self{Node: shortName(st.Self.DNSName), Addrs: st.TailscaleIPs})
	}
	for _, p := range st.Peer {
		nodes = append(nodes, Self{Node: shortName(p.DNSName), Addrs: p.TailscaleIPs})
	}

	for _, node := range nodes {
		if len(node.Addrs) == 0 {
			continue
		}
		peer, err := c.WhoIs(ctx, net.JoinHostPort(node.Addrs[0].String(), "0"))
		if err != nil {
			return "", fmt.Errorf("finding the Relay: WhoIs for %s: %w", node.Node, err)
		}
		peers = append(peers, peer)
	}
	return relayFrom(peers)
}

func relayFrom(peers []Peer) (string, error) {
	var names []string
	for _, p := range peers {
		if p.IsRelay() {
			names = append(names, p.Node)
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	switch len(names) {
	case 0:
		return "", fmt.Errorf("no Relay found in the Tailscale policy")
	case 1:
		return names[0], nil
	default:
		return "", fmt.Errorf("multiple Relays found in the Tailscale policy: %v; baf needs exactly one Relay", names)
	}
}
