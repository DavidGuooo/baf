package door

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestMetadataCannotReachPrivateAddresses(t *testing.T) {
	for _, raw := range []string{
		"127.0.0.1", "::1", "::ffff:127.0.0.1", "100.64.0.1", "10.0.0.1", "192.168.1.1",
		"169.254.169.254", "fd7a:115c:a1e0::1", "0.0.0.0", "224.0.0.1",
	} {
		if publicIP(netip.MustParseAddr(raw)) {
			t.Errorf("accepted %s", raw)
		}
	}
	if !publicIP(netip.MustParseAddr("8.8.8.8")) {
		t.Fatal("rejected public IP")
	}
	h := newHarness(t)
	h.s.fetch = metadataClient()
	for _, id := range []string{
		"https://127.0.0.1/client.json", "https://100.64.0.1/client.json", "https://[::1]/client.json",
		"http://chatgpt.com/client.json", "https://chatgpt.com:8444/client.json",
	} {
		if _, err := h.s.lookupClient(context.Background(), id); err == nil {
			t.Fatalf("accepted %s", id)
		}
	}
}

func TestMetadataValidation(t *testing.T) {
	for _, kind := range []string{"wrong id", "error status", "too large", "invalid redirect"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t)
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch kind {
				case "wrong id":
					writeJSON(w, 200, client{ID: "https://other.example/client.json", Redirects: []string{redirectURI}})
				case "error status":
					w.WriteHeader(404)
					writeJSON(w, 404, client{ID: clientID, Redirects: []string{redirectURI}})
				case "too large":
					w.Write([]byte(strings.Repeat("x", maxRequest+1)))
				case "invalid redirect":
					writeJSON(w, 200, client{ID: clientID, Redirects: []string{"javascript:alert(1)"}})
				}
			}))
			defer srv.Close()
			transport := srv.Client().Transport
			h.s.fetch = &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
				req, _ := http.NewRequestWithContext(r.Context(), "GET", srv.URL, nil)
				return transport.RoundTrip(req)
			})}
			if _, err := h.s.lookupClient(context.Background(), clientID); err == nil {
				t.Fatal("accepted bad metadata")
			}
		})
	}
}

func TestVendorMatch(t *testing.T) {
	for _, tc := range []struct {
		id, redirect string
		want         bool
	}{
		{clientID, redirectURI, true},
		{"opaque-id", redirectURI, true},
		{"https://openai.com/client.json", "https://sub.chatgpt.com/callback", true},
		{"https://evil.example/client.json", redirectURI, false},
		{clientID, "https://chatgpt.com.evil.example/callback", false},
		{clientID, "https://evilchatgpt.com/callback", false},
	} {
		if got := vendorMatches("openai/dot", client{ID: tc.id}, tc.redirect); got != tc.want {
			t.Errorf("%s %s = %v", tc.id, tc.redirect, got)
		}
	}
}
