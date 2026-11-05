// Package tailnet asks the local Tailscale for facts baf needs: this
// machine's name and addresses, where another machine is, and who is
// connecting with what permission. It uses Tailscale's own Go client rather
// than the tailscale command (ADR-0005).
package tailnet

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"

	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/DavidGuooo/baf/internal/runfolder"

	"tailscale.com/client/local"
	"tailscale.com/tailcfg"
	"tailscale.com/tailcfg/peercap"
)

// Capability is baf's one app permission in the Tailscale policy (ADR-0003).
// Every baf rule is a field of it.
const Capability = "github.com/DavidGuooo/baf/msg"

// Grant is one value of the baf permission, as written in a policy grant:
//
//	"app": {"github.com/DavidGuooo/baf/msg": [{"to": ["codex", "claude"]}]}
type Grant struct {
	Dirs  []string            `json:"dirs,omitempty"`  // run folders on the receiver; "*" = any
	To    []string            `json:"to,omitempty"`    // agents the sender may message; "*" = any
	Relay bool                `json:"relay,omitempty"` // may hold Door agents and forward for them
	Door  map[string][]string `json:"door,omitempty"`  // Door agent address -> agents it may message
}

// MayRunIn checks an already resolved folder against the sender's grants.
func (p Peer) MayRunIn(dir, home string) bool {
	for _, grant := range p.Grants {
		for _, entry := range grant.Dirs {
			if entry == "*" {
				return true
			}
			root, err := runfolder.Resolve(entry, home)
			if err == nil && runfolder.Contains(root, dir) {
				return true
			}
		}
	}
	return false
}

// Peer is who is connecting, as Tailscale reports it.
type Peer struct {
	Node   string   // MagicDNS name without the tailnet suffix, e.g. "grokbot"
	Tags   []string // e.g. ["tag:grokbot"]; empty for a person's own device
	Login  string   // the owner's login, or "tagged-devices"
	Grants []Grant  // every baf permission value that applies to this connection
}

// MayMessage reports whether any grant lets the peer message agentName.
// No grant at all means no access (default deny, ADR-0003).
func (p Peer) MayMessage(agentName string) bool {
	for _, g := range p.Grants {
		if slices.Contains(g.To, "*") || slices.Contains(g.To, agentName) {
			return true
		}
	}
	return false
}

var malformedDoors sync.Map // log each malformed policy name only once

func validDoor(name string) bool {
	if _, err := envelope.ParseAddress(name); err != nil {
		if _, seen := malformedDoors.LoadOrStore(name, true); !seen {
			slog.Warn("ignoring malformed Door agent in Tailscale policy", "name", name, "err", err)
		}
		return false
	}
	return true
}

// DoorAgents lists the Door agents this peer holds, once each, in name order.
// Only a grant with relay: true can name Door agents.
func (p Peer) DoorAgents() []string {
	var names []string
	for _, g := range p.Grants {
		for name := range g.Door {
			if validDoor(name) && g.Relay {
				names = append(names, name)
			}
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// DoorMayMessage checks the Door agent's own permission, never the Relay's To.
func (p Peer) DoorMayMessage(doorAgent, agentName string) bool {
	if !slices.Contains(p.DoorAgents(), doorAgent) {
		return false
	}
	for _, g := range p.Grants {
		if g.Relay && (slices.Contains(g.Door[doorAgent], "*") || slices.Contains(g.Door[doorAgent], agentName)) {
			return true
		}
	}
	return false
}

// Client wraps Tailscale's local API.
type Client struct {
	lc local.Client
}

// Self is this machine on the tailnet.
type Self struct {
	Node    string       // e.g. "imac"
	DNSName string       // full name used by this machine's HTTPS certificate
	Addrs   []netip.Addr // its Tailscale addresses
}

// Self returns this machine's name and Tailscale addresses.
func (c *Client) Self(ctx context.Context) (Self, error) {
	st, err := c.lc.StatusWithoutPeers(ctx)
	if err != nil {
		return Self{}, err
	}
	if st.Self == nil || st.Self.DNSName == "" {
		return Self{}, errors.New("tailscale reports no name for this machine; is it logged in?")
	}
	return Self{Node: shortName(st.Self.DNSName), DNSName: strings.TrimSuffix(st.Self.DNSName, "."), Addrs: st.TailscaleIPs}, nil
}

// GetCertificate uses Tailscale's local certificate service for sign-in HTTPS.
func (c *Client) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	return c.lc.GetCertificate(hello)
}

// SelfPeer reads this node's own grants through WhoIs, just like a caller's.
// Ask afresh so policy changes take effect without restarting baf.
func (c *Client) SelfPeer(ctx context.Context) (Peer, error) {
	self, err := c.Self(ctx)
	if err != nil {
		return Peer{}, err
	}
	if len(self.Addrs) == 0 {
		return Peer{}, errors.New("tailscale has no address for this machine yet")
	}
	return c.WhoIs(ctx, net.JoinHostPort(self.Addrs[0].String(), "0"))
}

// ErrNoSuchNode means no machine with that name is on the tailnet.
var ErrNoSuchNode = errors.New("no machine with that name on your tailnet")

// Lookup returns the Tailscale address of the machine named node, and
// whether Tailscale currently sees it online.
func (c *Client) Lookup(ctx context.Context, node string) (netip.Addr, bool, error) {
	st, err := c.lc.Status(ctx)
	if err != nil {
		return netip.Addr{}, false, err
	}
	if st.Self != nil && shortName(st.Self.DNSName) == node && len(st.TailscaleIPs) > 0 {
		return st.TailscaleIPs[0], true, nil
	}
	for _, p := range st.Peer {
		if shortName(p.DNSName) == node && len(p.TailscaleIPs) > 0 {
			return p.TailscaleIPs[0], p.Online, nil
		}
	}
	return netip.Addr{}, false, fmt.Errorf("%w: %q", ErrNoSuchNode, node)
}

// WhoIs asks Tailscale who is connecting from remoteAddr ("ip:port") and
// which baf permissions the policy gives that connection.
func (c *Client) WhoIs(ctx context.Context, remoteAddr string) (Peer, error) {
	res, err := c.lc.WhoIs(ctx, remoteAddr)
	if err != nil {
		return Peer{}, err
	}
	if res.Node == nil {
		return Peer{}, errors.New("tailscale returned no node")
	}
	grants, err := tailcfg.UnmarshalCapJSON[Grant](res.CapMap, peercap.Cap(Capability))
	if err != nil {
		return Peer{}, fmt.Errorf("reading %s from the policy: %w", Capability, err)
	}
	p := Peer{Node: shortName(res.Node.Name), Tags: res.Node.Tags, Grants: grants}
	if res.UserProfile != nil {
		p.Login = res.UserProfile.LoginName
	}
	return p, nil
}

// shortName turns "grokbot.example.ts.net." into "grokbot".
func shortName(dnsName string) string {
	name, _, _ := strings.Cut(dnsName, ".")
	return name
}
