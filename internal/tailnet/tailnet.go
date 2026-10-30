// Package tailnet asks the local Tailscale for facts baf needs: this
// machine's name and addresses, where another machine is, and who is
// connecting with what permission. It uses Tailscale's own Go client rather
// than the tailscale command (ADR-0005).
package tailnet

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

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
	To    []string `json:"to,omitempty"`    // agents the sender may message; "*" = any
	Relay bool     `json:"relay,omitempty"` // may forward for others (not used yet)
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

// Client wraps Tailscale's local API.
type Client struct {
	lc local.Client
}

// Self is this machine on the tailnet.
type Self struct {
	Node  string       // e.g. "imac"
	Addrs []netip.Addr // its Tailscale addresses
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
	return Self{Node: shortName(st.Self.DNSName), Addrs: st.TailscaleIPs}, nil
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
