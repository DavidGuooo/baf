package door

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DavidGuooo/baf/internal/agent"
	"github.com/DavidGuooo/baf/internal/mailbox"
	"github.com/DavidGuooo/baf/internal/tailnet"
)

const clientID = "https://chatgpt.com/oauth/client.json"
const redirectURI = "https://chatgpt.com/connector_platform_oauth_redirect"
const verifier = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

type fakeIdentity struct {
	mu              sync.Mutex
	tagged, removed bool
	login           string
}

func (f *fakeIdentity) WhoIs(_ context.Context, addr string) (tailnet.Peer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := tailnet.Peer{Node: "owners-phone", Login: f.login}
	if f.tagged {
		p.Tags = []string{"tag:agent"}
	}
	return p, nil
}
func (f *fakeIdentity) SelfPeer(context.Context) (tailnet.Peer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	doors := map[string][]string{"openai/dot": {"me", "codex"}, "openai/other": {"me"}}
	if f.removed {
		delete(doors, "openai/dot")
	}
	return tailnet.Peer{Grants: []tailnet.Grant{{Relay: true, To: []string{"*"}, Door: doors}}}, nil
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type harness struct {
	t              *testing.T
	s              *Server
	mailbox        *mailbox.Mailbox
	public, signin *httptest.Server
	browser        *http.Client
	identity       *fakeIdentity
	clock          atomic.Int64
	path           string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	f := &fakeIdentity{login: "owner@example.com"}
	home := t.TempDir()
	mb, err := mailbox.Open(mailbox.Options{
		Path: filepath.Join(home, "mailbox.db"), Node: "relay",
		Agents: map[string]agent.Adapter{"me": nil, "codex": nil, "claude": nil}, Door: f,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mb.Close() })
	h := &harness{t: t, mailbox: mb, identity: f, path: filepath.Join(home, "door.db")}
	h.public = httptest.NewUnstartedServer(nil)
	h.signin = httptest.NewUnstartedServer(nil)
	s, err := Open(Options{
		Path: h.path, PublicURL: "https://" + h.public.Listener.Addr().String(),
		SigninURL: "https://" + h.signin.Listener.Addr().String(), Policy: mb, Identity: f, Mailbox: mb,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.s = s
	h.clock.Store(time.Now().Unix())
	s.now = func() time.Time { return time.Unix(h.clock.Load(), 0) }
	metadata := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, client{ID: clientID, Name: "ChatGPT", Redirects: []string{redirectURI}})
	}))
	t.Cleanup(metadata.Close)
	transport := metadata.Client().Transport
	s.fetch = &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		u, _ := url.Parse(metadata.URL)
		clone.URL = u
		return transport.RoundTrip(clone)
	})}
	h.public.Config.Handler = s.PublicHandler()
	h.signin.Config.Handler = s.SigninHandler()
	h.public.StartTLS()
	h.signin.StartTLS()
	jar, _ := cookiejar.New(nil)
	h.browser = h.signin.Client()
	h.browser.Jar = jar
	h.browser.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	t.Cleanup(func() { h.public.Close(); h.signin.Close(); h.s.Close() })
	return h
}

func (h *harness) request(c *http.Client, method, target, body string,
	headers map[string]string,
) (*http.Response, string) {
	h.t.Helper()
	req, err := http.NewRequest(method, target, strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		h.t.Fatal(err)
	}
	return resp, string(data)
}

func (h *harness) query(name string) url.Values {
	return url.Values{
		"client_id": {clientID}, "redirect_uri": {redirectURI}, "response_type": {"code"},
		"code_challenge_method": {"S256"}, "code_challenge": {challenge(verifier)},
		"resource": {h.s.resource(name)}, "scope": {"baf"}, "state": {"keep-this-state"},
	}
}

func (h *harness) page(q url.Values) (int, string, string) {
	h.t.Helper()
	resp, body := h.request(h.browser, "GET", h.signin.URL+"/authorize?"+q.Encode(), "", nil)
	match := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(body)
	csrf := ""
	if len(match) == 2 {
		csrf = match[1]
	}
	return resp.StatusCode, csrf, body
}

func (h *harness) allow(csrf string) (*http.Response, string) {
	h.t.Helper()
	form := url.Values{"csrf": {csrf}, "decision": {"allow"}}
	return h.request(h.browser, "POST", h.signin.URL+"/authorize", form.Encode(),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
}

func (h *harness) code(name string) string {
	h.t.Helper()
	status, csrf, body := h.page(h.query(name))
	if status != 200 || csrf == "" || !strings.Contains(body, "owner@example.com") || !strings.Contains(body, name) {
		h.t.Fatalf("Allow page: %d %s", status, body)
	}
	if strings.Contains(body, `name="days"`) || strings.Contains(body, "Connector lasts") {
		h.t.Fatal("Allow page still asks for a connector duration")
	}
	resp, body := h.allow(csrf)
	if resp.StatusCode != 302 {
		h.t.Fatalf("Allow: %d %s", resp.StatusCode, body)
	}
	back, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		h.t.Fatal(err)
	}
	q := back.Query()
	if q.Get("iss") != h.s.publicURL || q.Get("state") != "keep-this-state" || q.Get("code") == "" {
		h.t.Fatalf("redirect = %s", back)
	}
	if strings.Contains(back.String(), "owner") || strings.Contains(body, "owner@example.com") {
		h.t.Fatal("owner identity leaked in redirect")
	}
	return q.Get("code")
}

func codeForm(code string) url.Values {
	return url.Values{
		"grant_type": {"authorization_code"}, "client_id": {clientID}, "redirect_uri": {redirectURI},
		"code": {code}, "code_verifier": {verifier},
	}
}
func refreshForm(tok tokens) url.Values {
	return url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {tok.Refresh}}
}

func (h *harness) token(q url.Values, want int) tokens {
	h.t.Helper()
	resp, body := h.request(h.public.Client(), "POST", h.public.URL+"/token", q.Encode(),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if resp.StatusCode != want {
		h.t.Fatalf("token status %d want %d: %s", resp.StatusCode, want, body)
	}
	var tok tokens
	if want == 200 {
		if err := json.Unmarshal([]byte(body), &tok); err != nil {
			h.t.Fatal(err)
		}
		if tok.Access == "" || tok.Refresh == "" || tok.Type != "Bearer" || tok.ExpiresIn <= 0 || tok.ExpiresIn > 86400 {
			h.t.Fatalf("bad token response: %+v", tok)
		}
		if resp.Header.Get("Cache-Control") != "no-store" || strings.Contains(body, "owner@example.com") {
			h.t.Fatal("token response leaked identity or is cacheable")
		}
	}
	return tok
}

func (h *harness) bearer(name, token string, want int) {
	h.t.Helper()
	resp, body := h.request(h.public.Client(), "POST", h.public.URL+"/door/"+name+"/mcp",
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"Authorization": "Bearer " + token})
	if resp.StatusCode != want {
		h.t.Fatalf("MCP %s: %d want %d: %s", name, resp.StatusCode, want, body)
	}
	metadata := h.s.publicURL + "/.well-known/oauth-protected-resource/door/" + name + "/mcp"
	if want == 401 && !strings.Contains(resp.Header.Get("WWW-Authenticate"), `resource_metadata="`+metadata+`"`) {
		h.t.Fatalf("bad challenge: %s", resp.Header.Get("WWW-Authenticate"))
	}
}

func TestOAuthRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.bearer("openai/dot", "", 401)
	for path, pair := range rangeMetadata(h) {
		field, want := pair[0], pair[1]
		resp, body := h.request(h.public.Client(), "GET", h.public.URL+path, "", nil)
		var v map[string]any
		if resp.StatusCode != 200 || json.Unmarshal([]byte(body), &v) != nil || v[field] != want {
			t.Fatalf("metadata %s: %d %s", path, resp.StatusCode, body)
		}
	}
	code := h.code("openai/dot")
	q := codeForm(code)
	q.Set("resource", h.s.resource("openai/dot"))
	tok := h.token(q, 200)
	h.bearer("openai/dot", tok.Access, 200)
	h.bearer("openai/other", tok.Access, 401)
	h.token(q, 400)
	next := h.token(refreshForm(tok), 200)
	if next.Refresh == tok.Refresh || next.Access == tok.Access {
		t.Fatal("tokens did not rotate")
	}
	h.token(refreshForm(tok), 400)
	h.bearer("openai/dot", tok.Access, 200) // access stays valid for its original day
	h.bearer("openai/dot", next.Access, 200)
	list, err := h.s.List()
	if err != nil || len(list) != 2 || !list[0].Connected || list[0].ApprovedBy != "owner@example.com" ||
		list[0].ClientID != clientID || list[1].Connected {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	// No raw token or code is stored, including in SQLite's journal.
	for _, path := range []string{h.path, h.path + "-wal", h.path + "-shm"} {
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{code, tok.Access, tok.Refresh, next.Access, next.Refresh} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("raw credential in %s", path)
			}
		}
	}
}

func rangeMetadata(h *harness) map[string][2]string {
	return map[string][2]string{
		"/.well-known/oauth-protected-resource/door/openai/dot/mcp": {"resource", h.s.resource("openai/dot")},
		"/.well-known/oauth-authorization-server":                   {"authorization_endpoint", h.signin.URL + "/authorize"},
	}
}

func TestAuthorizeRefusals(t *testing.T) {
	cases := []struct {
		name   string
		change func(*harness, url.Values)
		want   int
	}{
		{"tagged", func(h *harness, q url.Values) {
			h.identity.mu.Lock()
			h.identity.tagged = true
			h.identity.mu.Unlock()
		}, 403},
		{"unlisted", func(h *harness, q url.Values) { q.Set("resource", h.s.resource("openai/missing")) }, 403},
		{"missing PKCE", func(h *harness, q url.Values) { q.Del("code_challenge") }, 400},
		{"plain PKCE", func(h *harness, q url.Values) { q.Set("code_challenge_method", "plain") }, 400},
		{"bad PKCE", func(h *harness, q url.Values) { q.Set("code_challenge", "short") }, 400},
		{"foreign resource", func(h *harness, q url.Values) {
			q.Set("resource", "https://elsewhere/door/openai/dot/mcp")
		}, 400},
		{"wrong redirect", func(h *harness, q url.Values) {
			q.Set("redirect_uri", "https://attacker.example/callback")
		}, 400},
		{"duplicate parameter", func(h *harness, q url.Values) { q.Add("resource", h.s.resource("openai/other")) }, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			q := h.query("openai/dot")
			tc.change(h, q)
			status, _, body := h.page(q)
			if status != tc.want {
				t.Fatalf("%d want %d: %s", status, tc.want, body)
			}
		})
	}
}

func TestTokenBindings(t *testing.T) {
	for _, field := range []string{"code_verifier", "client_id", "redirect_uri", "resource"} {
		t.Run(field, func(t *testing.T) {
			h := newHarness(t)
			q := codeForm(h.code("openai/dot"))
			q.Set(field, "wrong")
			h.token(q, 400)
		})
	}
	for _, field := range []string{"client_id", "resource"} {
		t.Run("refresh "+field, func(t *testing.T) {
			h := newHarness(t)
			tok := h.token(codeForm(h.code("openai/dot")), 200)
			q := refreshForm(tok)
			q.Set(field, "wrong")
			h.token(q, 400)
			h.token(refreshForm(tok), 200)
		})
	}
}

func TestExpiredRevokedReplacedAndRemoved(t *testing.T) {
	for _, kind := range []string{"expired access", "revoked", "replaced", "removed"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t)
			tok := h.token(codeForm(h.code("openai/dot")), 200)
			switch kind {
			case "expired access":
				h.clock.Add(int64((24 * time.Hour) / time.Second))
			case "revoked":
				if err := h.s.Revoke("openai/dot"); err != nil {
					t.Fatal(err)
				}
			case "replaced":
				h.code("openai/dot") // approval replaces immediately, before exchange
			case "removed":
				h.identity.mu.Lock()
				h.identity.removed = true
				h.identity.mu.Unlock()
			}
			h.bearer("openai/dot", tok.Access, 401)
			want := 400
			if kind == "expired access" {
				want = 200
			}
			next := h.token(refreshForm(tok), want)
			if want == 200 {
				h.bearer("openai/dot", next.Access, 200)
			}
		})
	}
}

func TestPendingCodes(t *testing.T) {
	for _, kind := range []string{"expired", "replaced", "revoked", "removed"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t)
			code := h.code("openai/dot")
			switch kind {
			case "expired":
				h.clock.Add(300)
			case "replaced":
				h.code("openai/dot")
			case "revoked":
				if err := h.s.Revoke("openai/dot"); err != nil {
					t.Fatal(err)
				}
			case "removed":
				h.identity.mu.Lock()
				h.identity.removed = true
				h.identity.mu.Unlock()
			}
			h.token(codeForm(code), 400)
		})
	}
}

func TestAllowGuard(t *testing.T) {
	for _, kind := range []string{
		"missing", "cookie missing", "wrong person", "tagged on POST", "expired", "replayed", "policy changed",
	} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t)
			_, csrf, _ := h.page(h.query("openai/dot"))
			switch kind {
			case "missing":
				csrf = ""
			case "cookie missing":
				h.browser.Jar = nil
			case "wrong person":
				h.identity.mu.Lock()
				h.identity.login = "another@example.com"
				h.identity.mu.Unlock()
			case "tagged on POST":
				h.identity.mu.Lock()
				h.identity.tagged = true
				h.identity.mu.Unlock()
			case "expired":
				h.clock.Add(600)
			case "replayed":
				resp, _ := h.allow(csrf)
				if resp.StatusCode != 302 {
					t.Fatal(resp.Status)
				}
			case "policy changed":
				h.identity.mu.Lock()
				h.identity.removed = true
				h.identity.mu.Unlock()
			}
			resp, body := h.allow(csrf)
			if resp.StatusCode != 403 {
				t.Fatalf("%d: %s", resp.StatusCode, body)
			}
		})
	}
}

func TestPublicRoutesAndLimits(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{
		"/", "/.env", "/authorize", "/.well-known/openid-configuration", "/token/", "/door/openai/dot/mcp/",
		"//door/openai/dot/mcp", "/door/openai/dot/../dot/mcp", "/door/openai%2Fdot/mcp",
	} {
		resp, body := h.request(h.public.Client(), "GET", h.public.URL+path, "", nil)
		if resp.StatusCode != 404 || body != "" || resp.Header.Get("Location") != "" {
			t.Fatalf("%s: %d %q", path, resp.StatusCode, body)
		}
	}
	for _, path := range []string{"/token", "/register", "/door/openai/dot/mcp"} {
		limit := maxRequest
		if strings.HasSuffix(path, "/mcp") {
			limit = maxMCPRequest
		}
		resp, _ := h.request(h.public.Client(), "POST", h.public.URL+path, strings.Repeat("x", limit+1), nil)
		if resp.StatusCode != 413 {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
	}
	resp, _ := h.request(h.browser, "POST", h.signin.URL+"/authorize", strings.Repeat("x", maxRequest+1), nil)
	if resp.StatusCode != 413 {
		t.Fatal(resp.Status)
	}
}

func TestDynamicRegistrationAndVendorWarning(t *testing.T) {
	h := newHarness(t)
	resp, body := h.request(h.public.Client(), "POST", h.public.URL+"/register",
		`{"client_name":"Other client","redirect_uris":["https://other.example/callback"]}`,
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != 201 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var c client
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatal(err)
	}
	q := h.query("openai/dot")
	q.Set("client_id", c.ID)
	q.Set("redirect_uri", c.Redirects[0])
	status, csrf, page := h.page(q)
	if status != 200 || !strings.Contains(page, "Warning:") {
		t.Fatalf("%d %s", status, page)
	}
	resp, body = h.allow(csrf)
	if resp.StatusCode != 302 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	back, _ := url.Parse(resp.Header.Get("Location"))
	form := codeForm(back.Query().Get("code"))
	form.Set("client_id", c.ID)
	form.Set("redirect_uri", c.Redirects[0])
	tok := h.token(form, 200)
	h.bearer("openai/dot", tok.Access, 200)
	h.clock.Add(7 * 86400)
	h.bearer("openai/dot", tok.Access, 401)
}

func TestRestartKeepsConnector(t *testing.T) {
	h := newHarness(t)
	tok := h.token(codeForm(h.code("openai/dot")), 200)
	// Use another instance to read the durable state, without changing handlers.
	other, err := Open(Options{
		Path: h.path, PublicURL: h.s.publicURL, SigninURL: h.s.signinURL,
		Policy: h.s.policy, Identity: h.identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if name, err := other.Authenticate(tok.Access, h.s.resource("openai/dot")); err != nil || name != "openai/dot" {
		t.Fatalf("%q %v", name, err)
	}
	if err := other.Revoke("openai/dot"); err != nil {
		t.Fatal(err)
	}
	h.bearer("openai/dot", tok.Access, 401)
}

func TestConcurrentRefreshSingleUse(t *testing.T) {
	h := newHarness(t)
	tok := h.token(codeForm(h.code("openai/dot")), 200)
	results := make(chan int, 2)
	for range 2 {
		go func() {
			resp, err := h.public.Client().PostForm(h.public.URL+"/token", refreshForm(tok))
			if err != nil {
				results <- 0
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			results <- resp.StatusCode
		}()
	}
	a, b := <-results, <-results
	if !((a == 200 && b == 400) || (a == 400 && b == 200)) {
		t.Fatal(fmt.Sprintf("concurrent refresh: %d, %d", a, b))
	}
}

func TestConnectorDoesNotExpire(t *testing.T) {
	h := newHarness(t)
	tok := h.token(codeForm(h.code("openai/dot")), 200)
	h.clock.Add(366 * 86400)
	h.bearer("openai/dot", tok.Access, 401)
	tok = h.token(refreshForm(tok), 200)
	if tok.ExpiresIn != 86400 {
		t.Fatalf("access duration = %d, want 86400", tok.ExpiresIn)
	}
	h.bearer("openai/dot", tok.Access, 200)
	h.clock.Add(86400)
	h.bearer("openai/dot", tok.Access, 401)
	h.token(refreshForm(tok), 200)
}

func TestLegacyConnectorExpiryIgnored(t *testing.T) {
	for _, exchanged := range []bool{false, true} {
		t.Run(fmt.Sprintf("exchanged=%v", exchanged), func(t *testing.T) {
			h := newHarness(t)
			code := h.code("openai/dot")
			var storedExpiry int64
			if err := h.s.db.QueryRow(`SELECT expires_at FROM connectors`).Scan(&storedExpiry); err != nil || storedExpiry != 0 {
				t.Fatalf("new expiry = %d, err = %v", storedExpiry, err)
			}
			var tok tokens
			if exchanged {
				tok = h.token(codeForm(code), 200)
			}
			// Simulate the old row's one-year deadline passing. Opening the
			// database must keep its code and tokens without rewriting the row.
			oldExpiry := h.s.now().Unix() - 1
			if _, err := h.s.db.Exec(`UPDATE connectors SET expires_at=?`, oldExpiry); err != nil {
				t.Fatal(err)
			}
			other, err := Open(Options{Path: h.path, PublicURL: h.s.publicURL, SigninURL: h.s.signinURL,
				Policy: h.s.policy, Identity: h.identity})
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			other.now = h.s.now
			if err := other.db.QueryRow(`SELECT expires_at FROM connectors`).Scan(&storedExpiry); err != nil || storedExpiry != oldExpiry {
				t.Fatalf("legacy row changed: %d, err = %v", storedExpiry, err)
			}
			if exchanged {
				if _, err := other.Authenticate(tok.Access, h.s.resource("openai/dot")); err != nil {
					t.Fatalf("legacy access token refused: %v", err)
				}
				tok, err = other.exchange(refreshForm(tok))
			} else {
				tok, err = other.exchange(codeForm(code))
			}
			if err != nil || tok.ExpiresIn != 86400 {
				t.Fatalf("legacy exchange: expires_in=%d err=%v", tok.ExpiresIn, err)
			}
			h.bearer("openai/dot", tok.Access, 200)
		})
	}
}

func TestConnectorStates(t *testing.T) {
	h := newHarness(t)
	check := func(want string) {
		t.Helper()
		list, err := h.s.List()
		if err != nil || len(list) != 2 || list[0].State != want || list[0].Connected != (want == "connected") {
			t.Fatalf("want %q, list=%+v err=%v", want, list, err)
		}
	}
	check("none")
	h.code("openai/dot")
	check("waiting for ChatGPT")
	h.clock.Add(300)
	check("sign-in expired")
	code := h.code("openai/dot")
	check("waiting for ChatGPT")
	h.token(codeForm(code), 200)
	h.clock.Add(366 * 86400) // Only the access token expires.
	check("connected")
	if err := h.s.Revoke("openai/dot"); err != nil {
		t.Fatal(err)
	}
	check("none")
	// The name alone does not identify the app; a non-OpenAI client can use it.
	_, err := h.s.approve(grant{Connector: Connector{Agent: "openai/dot", ClientID: "https://example.com/client.json"},
		Redirect: "https://example.com/callback"})
	if err != nil {
		t.Fatal(err)
	}
	check("waiting for the app")
}

// Browsers apply form-action to the redirect after Allow, so the page must
// allow the client's redirect origin or the owner is left on the page.
func TestAllowPageLetsTheRedirectThrough(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.request(h.browser, "GET", h.signin.URL+"/authorize?"+h.query("openai/dot").Encode(), "", nil)
	u, _ := url.Parse(redirectURI)
	want := "form-action 'self' https://" + u.Host + ";"
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, want) {
		t.Fatalf("CSP %q lacks %q", csp, want)
	}
}

func TestFormActionRefusesOddHosts(t *testing.T) {
	for _, redirect := range []string{"https://a.example; script-src *", "http://chatgpt.com/x", "::"} {
		if got := formAction(redirect); got != "'self'" {
			t.Errorf("formAction(%q) = %q, want 'self'", redirect, got)
		}
	}
}
