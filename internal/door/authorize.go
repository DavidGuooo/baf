package door

import (
	"context"
	"crypto/subtle"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"time"
)

type approval struct {
	Grant       grant
	State, Node string
	Expires     time.Time
}

var allowPage = template.Must(template.New("allow").Parse(`<!doctype html>
<html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>Allow a baf connector?</title>
<style>body{font:16px system-ui;max-width:36rem;margin:3rem auto;padding:0 1rem;overflow-wrap:anywhere}` +
	`dt{margin-top:1rem;color:#555}dd{margin-left:0}button{font:inherit;padding:.4rem}</style>
<h1>Allow {{.Agent}} to use baf?</h1>
<dl><dt>Approving as</dt><dd>{{.Login}}</dd><dt>Client</dt><dd>{{.Client.Name}} ({{.Client.ID}})</dd>
<dt>Returns to</dt><dd>{{.Redirect}}</dd></dl>
{{if .Warn}}<p><strong>Warning:</strong> This client's identity or redirect does not match ` +
	`the Door agent's vendor. Allow only if you intended to connect this client.</p>{{end}}
<p>What this Door agent may message is set in the Tailscale policy. Allow replaces its earlier connector.</p>
<form method="post" action="/authorize"><input type="hidden" name="csrf" value="{{.CSRF}}">
<p><button name="decision" value="allow">Allow</button> ` +
	`<button name="decision" value="deny">Deny</button></p></form></html>`))

const csrfCookie = "__Host-baf-allow"

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	peer, err := s.identity.WhoIs(ctx, r.RemoteAddr)
	cancel()
	// Only a person on the actual tailnet connection may approve a connector.
	if err != nil || len(peer.Tags) > 0 || peer.Login == "" || peer.Login == "tagged-devices" {
		slog.Info("door sign-in denied", "approver", peer.Login, "reason", "a person's tailnet identity is required")
		http.Error(w, "approve from a person's device on the tailnet; tagged devices cannot Allow", 403)
		return
	}
	if r.Method == "POST" {
		s.allow(w, r, peer.Login, peer.Node)
		return
	}
	name, clientID, reason := "", "", "invalid authorization query"
	defer func() {
		if reason != "" {
			slog.Info("door sign-in denied", "agent", name, "approver", peer.Login,
				"client_id", logURL(clientID), "reason", reason)
		}
	}()
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || !singleValues(q) {
		http.Error(w, "invalid authorization query", 400)
		return
	}
	var ok bool
	name, ok = s.resourceAgent(q.Get("resource"))
	clientID = q.Get("client_id")
	if !ok {
		reason = "invalid resource"
		http.Error(w, "resource must be this Relay's /door/vendor/name/mcp URL", 400)
		return
	}
	if err := s.allowed(name); err != nil {
		reason = string(policyRefusal(err, "name no longer in the policy"))
		http.Error(w, err.Error(), 403)
		return
	}
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" ||
		!validChallenge(q.Get("code_challenge")) {
		reason = "invalid response type or PKCE challenge"
		http.Error(w, "need response_type=code and a valid S256 PKCE challenge", 400)
		return
	}
	scope := q.Get("scope")
	if scope == "" {
		scope = "baf"
	}
	if scope != "baf" {
		reason = "unsupported scope"
		http.Error(w, "only the baf scope is supported", 400)
		return
	}
	c, err := s.lookupClient(r.Context(), q.Get("client_id"))
	if err != nil {
		reason = "client lookup failed"
		http.Error(w, err.Error(), 400)
		return
	}
	if !slices.Contains(c.Redirects, q.Get("redirect_uri")) {
		reason = "redirect_uri is not registered"
		http.Error(w, "redirect_uri is not registered for this client", 400)
		return
	}
	csrf := randomToken()
	a := approval{
		Grant: grant{
			Connector: Connector{Agent: name, ApprovedBy: peer.Login, ClientID: c.ID},
			Resource:  s.resource(name), Redirect: q.Get("redirect_uri"),
			Challenge: q.Get("code_challenge"), Scope: scope,
		},
		State: q.Get("state"), Node: peer.Node, Expires: s.now().Add(10 * time.Minute),
	}
	s.mu.Lock()
	for key, p := range s.pending {
		if !s.now().Before(p.Expires) {
			delete(s.pending, key)
		}
	}
	full := len(s.pending) >= 1024
	if !full {
		s.pending[string(hash(csrf))] = a
	}
	s.mu.Unlock()
	if full {
		reason = "too many pending sign-ins"
		http.Error(w, "too many pending sign-ins; try again shortly", 429)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookie, Value: csrf, Path: "/", Secure: true, HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: 600,
	})
	// Allow answers with a redirect to the client. Browsers apply form-action
	// to that redirect too, so 'self' alone would leave the owner stuck here.
	w.Header().Set("Content-Security-Policy", signinCSP(formAction(a.Grant.Redirect)))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	reason = ""
	slog.Info("door Allow page shown", "agent", name, "approver", peer.Login, "client_id", logURL(c.ID))
	allowPage.Execute(w, map[string]any{
		"Agent": name, "Login": peer.Login, "Client": c, "Redirect": a.Grant.Redirect,
		"Warn": !vendorMatches(name, c, a.Grant.Redirect), "CSRF": csrf,
	})
}

func (s *Server) allow(w http.ResponseWriter, r *http.Request, login, node string) {
	var a approval
	outcome, reason := "denied", "invalid Allow form"
	defer func() {
		slog.Info("door sign-in "+outcome, "agent", a.Grant.Agent, "approver", login,
			"client_id", logURL(a.Grant.ClientID), "reason", reason)
	}()
	if err := r.ParseForm(); err != nil || !singleValues(r.PostForm) {
		http.Error(w, "invalid Allow form", 400)
		return
	}
	q := r.PostForm
	csrf := q.Get("csrf")
	// Bind this one-use approval to the browser as well as the person and device.
	cookie, err := r.Cookie(csrfCookie)
	if err != nil || csrf == "" || subtle.ConstantTimeCompare([]byte(csrf), []byte(cookie.Value)) != 1 {
		reason = "invalid or missing Allow form guard"
		http.Error(w, "invalid or missing Allow form guard; open the sign-in page again", 403)
		return
	}
	key := string(hash(csrf))
	s.mu.Lock()
	var ok bool
	a, ok = s.pending[key]
	if ok && a.Grant.ApprovedBy == login && a.Node == node {
		delete(s.pending, key)
	} else {
		ok = false
	}
	s.mu.Unlock()
	if !ok || !s.now().Before(a.Expires) {
		reason = "sign-in expired or belongs to another device"
		http.Error(w, "sign-in expired or belongs to another device; open it again", 403)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookie, Path: "/", Secure: true, HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
	if err := s.allowed(a.Grant.Agent); err != nil {
		reason = string(policyRefusal(err, "name no longer in the policy"))
		http.Error(w, err.Error(), 403)
		return
	}
	back, _ := url.Parse(a.Grant.Redirect) // checked against client metadata on GET
	values := back.Query()
	values.Set("iss", s.publicURL)
	if a.State != "" {
		values.Set("state", a.State)
	}
	switch q.Get("decision") {
	case "deny":
		values.Set("error", "access_denied")
		reason = "approver denied"
	case "allow":
		a.Grant.ApprovedAt = s.now()
		code, err := s.approve(a.Grant)
		if err != nil {
			reason = "could not save connector"
			http.Error(w, "could not save connector", 500)
			return
		}
		values.Set("code", code)
		outcome, reason = "allowed", "approved"
	default:
		http.Error(w, "choose Allow or Deny", 400)
		return
	}
	back.RawQuery = values.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

// signinCSP is the Allow page's Content-Security-Policy: no scripts, no
// frames, and the form may go only to formAction.
func signinCSP(formAction string) string {
	return "default-src 'none'; style-src 'unsafe-inline'; form-action " + formAction +
		"; frame-ancestors 'none'; base-uri 'none'"
}

var cspHost = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]+)?$`)

// formAction allows the form to post here and to be redirected to the
// client's redirect origin (https://chatgpt.com for ChatGPT).
func formAction(redirect string) string {
	u, err := url.Parse(redirect)
	if err != nil || u.Scheme != "https" || !cspHost.MatchString(u.Host) {
		return "'self'"
	}
	return "'self' https://" + u.Host
}
