package tailnet

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/types/key"
)

func TestRelayDiscoveryFromLocalPolicy(t *testing.T) {
	for _, tc := range []struct {
		name         string
		self, other  bool
		want, reason string
	}{
		{"none", false, false, "", "no Relay found"},
		{"peer", false, true, "imac", ""},
		{"self", true, false, "grokbot", ""},
		{"two", true, true, "", "multiple Relays found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/localapi/v0/status":
					json.NewEncoder(w).Encode(ipnstate.Status{
						Self:         &ipnstate.PeerStatus{DNSName: "grokbot.example.ts.net."},
						TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.2")},
						Peer: map[key.NodePublic]*ipnstate.PeerStatus{key.NewNode().Public(): {
							DNSName: "imac.example.ts.net.", TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.1")},
						}},
					})
				case "/localapi/v0/whois":
					name, relay := "imac", tc.other
					switch r.URL.Query().Get("addr") {
					case "100.64.0.2:0":
						name, relay = "grokbot", tc.self
					case "100.64.0.1:0":
					default:
						t.Errorf("unexpected WhoIs %s", r.URL)
						http.Error(w, "bad address", 400)
						return
					}
					json.NewEncoder(w).Encode(map[string]any{"Node": map[string]string{"Name": name + ".example.ts.net."}, "CapMap": map[string]any{Capability: []Grant{{Relay: relay}}}})
				default:
					t.Errorf("unexpected path %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			c := &Client{lc: local.Client{OmitAuth: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "tcp", srv.Listener.Addr().String())
			}}}
			got, err := c.Relay(context.Background())
			if tc.reason != "" {
				if err == nil || !strings.Contains(err.Error(), tc.reason) {
					t.Fatalf("%s, %v", got, err)
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("%s, %v", got, err)
			}
			// Lookup must recognize this node too, so it can't become a Door vendor.
			addr, online, err := c.Lookup(context.Background(), "grokbot")
			if err != nil || !online || addr.String() != "100.64.0.2" {
				t.Fatal(fmt.Sprint(addr, online, err))
			}
		})
	}
}
