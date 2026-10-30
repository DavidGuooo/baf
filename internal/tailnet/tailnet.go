// Package tailnet asks the local Tailscale for facts baf needs. It uses
// Tailscale's own Go client rather than the tailscale command (ADR-0005).
package tailnet

import (
	"context"
	"errors"
	"strings"

	"tailscale.com/client/local"
)

// SelfNode returns this machine's MagicDNS name without the tailnet suffix,
// for example "imac". That is the node part of every local address.
func SelfNode(ctx context.Context) (string, error) {
	var lc local.Client
	st, err := lc.StatusWithoutPeers(ctx)
	if err != nil {
		return "", err
	}
	if st.Self == nil || st.Self.DNSName == "" {
		return "", errors.New("tailscale reports no name for this machine; is it logged in?")
	}
	name, _, _ := strings.Cut(st.Self.DNSName, ".")
	return name, nil
}
