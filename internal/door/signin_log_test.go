package door

import (
	"errors"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestExchangeRefusal(t *testing.T) {
	now := time.Unix(1000, 0)
	g := grant{Connector: Connector{ClientID: clientID}, Resource: "https://relay/door/openai/dot/mcp",
		Redirect: redirectURI, Challenge: challenge(verifier)}
	for _, tc := range []struct {
		field, value, want string
	}{
		{"client_id", "wrong", "client_id mismatch"},
		{"redirect_uri", "wrong", "redirect_uri mismatch"},
		{"resource", "https://other/door/openai/dot/mcp", "resource mismatch"},
		{"code_verifier", "short", "bad PKCE verifier"},
		{"code_verifier", verifier + "x", "bad PKCE verifier"},
		{"resource", g.Resource, ""},
		{"resource", "", ""},
	} {
		t.Run(tc.field+"="+tc.value, func(t *testing.T) {
			form := codeForm("unused")
			form.Set(tc.field, tc.value)
			if got := exchangeRefusal(g, form, now.Unix()+300, now); got != tc.want {
				t.Fatalf("reason = %q, want %q", got, tc.want)
			}
		})
	}
	if got := exchangeRefusal(g, codeForm("unused"), now.Unix(), now); got != "code expired" {
		t.Fatalf("expiry reason = %q", got)
	}
	// A refresh does not need a redirect or verifier, even after the code expired.
	if got := exchangeRefusal(g, refreshForm(tokens{Refresh: "unused"}), now.Unix()-1, now); got != "" {
		t.Fatalf("refresh refused: %s", got)
	}
}

func TestStoredGrantRefusals(t *testing.T) {
	h := newHarness(t)
	check := func(form url.Values, want string) {
		t.Helper()
		_, err := h.s.exchange(form)
		if !errors.Is(err, errGrant) || err.Error() != "invalid_grant" || refusalReason(err) != want {
			t.Fatalf("error = %v, reason = %q, want %q", err, refusalReason(err), want)
		}
	}
	check(codeForm("unknown"), "unknown or used code")
	check(refreshForm(tokens{Refresh: "unknown"}), "unknown or used refresh token")
	check(url.Values{"grant_type": {"password"}}, "unsupported grant type")
	code := h.code("openai/dot")
	tok := h.token(codeForm(code), 200)
	check(codeForm(code), "unknown or used code")
	h.token(refreshForm(tok), 200)
	check(refreshForm(tok), "unknown or used refresh token")
	code = h.code("openai/dot")
	h.identity.mu.Lock()
	h.identity.removed = true
	h.identity.mu.Unlock()
	check(codeForm(code), "name no longer in the policy")
	// The HTTP response carries no private diagnostic, including unsupported grants.
	resp, body := h.request(h.public.Client(), "POST", h.public.URL+"/token", "grant_type=password",
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if resp.StatusCode != 400 || body != "{\"error\":\"invalid_grant\"}\n" {
		t.Fatalf("token response: %d %s", resp.StatusCode, body)
	}
}

func TestBearerRefusalReasons(t *testing.T) {
	h := newHarness(t)
	tok := h.token(codeForm(h.code("openai/dot")), 200)
	check := func(token, resource, want string) {
		t.Helper()
		_, err := h.s.Authenticate(token, resource)
		if !errors.Is(err, errGrant) || refusalReason(err) != want {
			t.Fatalf("error = %v, reason = %q, want %q", err, refusalReason(err), want)
		}
	}
	check("unknown", h.s.resource("openai/dot"), "unknown")
	check(tok.Access, h.s.resource("openai/other"), "wrong resource")
	h.identity.mu.Lock()
	h.identity.removed = true
	h.identity.mu.Unlock()
	check(tok.Access, h.s.resource("openai/dot"), "not in policy")
	h.clock.Add(86400)
	check(tok.Access, h.s.resource("openai/dot"), "expired")
}

func TestBearerRefusalLogLimit(t *testing.T) {
	h := newHarness(t)
	h.bearer("openai/dot", "", 401)
	if len(h.s.bearerRefusals) != 0 {
		t.Fatal("token-less probe used the log allowance")
	}
	var logged atomic.Int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if h.s.shouldLogBearerRefusal("openai/dot") {
				logged.Add(1)
			}
		})
	}
	wg.Wait()
	if logged.Load() != 1 || !h.s.shouldLogBearerRefusal("openai/other") {
		t.Fatal("log limit is not once per Door agent")
	}
	h.clock.Add(59)
	if h.s.shouldLogBearerRefusal("openai/dot") {
		t.Fatal("logged twice within a minute")
	}
	h.clock.Add(1)
	if !h.s.shouldLogBearerRefusal("openai/dot") {
		t.Fatal("did not log after a minute")
	}
	for i := range 1100 {
		h.s.shouldLogBearerRefusal(fmt.Sprintf("unknown/agent%d", i))
	}
	if len(h.s.bearerRefusals) != 1024 || h.s.shouldLogBearerRefusal("openai/dot") {
		t.Fatal("unbounded bookkeeping or evicted a recent refusal")
	}
}

func TestLogURL(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{clientID, clientID},
		{"opaque-client-id", "opaque-client-id"},
		{"https://user:secret@example.com/mcp?code=secret#secret", "https://example.com/mcp"},
		{"https://example.com/mcp?", "https://example.com/mcp"},
		{"https://example.com/%zz?secret", "invalid URL"},
	} {
		if got := logURL(tc.raw); got != tc.want {
			t.Errorf("URL = %q, want %q", got, tc.want)
		}
	}
}
