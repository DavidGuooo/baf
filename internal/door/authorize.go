package door

import (
	"context"
	"crypto/subtle"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strconv"
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
<style>body{font:16px system-ui;max-width:36rem;margin:3rem auto;padding:0 1rem;overflow-wrap:anywhere}dt{margin-top:1rem;color:#555}dd{margin-left:0}button,select{font:inherit;padding:.4rem}</style>
<h1>Allow {{.Agent}} to use baf?</h1>
<dl><dt>Approving as</dt><dd>{{.Login}}</dd><dt>Client</dt><dd>{{.Client.Name}} ({{.Client.ID}})</dd>
<dt>Returns to</dt><dd>{{.Redirect}}</dd></dl>
{{if .Warn}}<p><strong>Warning:</strong> This client's identity or redirect does not match the Door agent's vendor. Allow only if you intended to connect this client.</p>{{end}}
<p>What this Door agent may message is set in the Tailscale policy. Allow replaces its earlier connector.</p>
<form method="post" action="/authorize"><input type="hidden" name="csrf" value="{{.CSRF}}">
<label>Connector lasts <select name="days"><option value="7">7 days</option><option value="30" selected>30 days</option><option value="90">90 days</option><option value="365">1 year</option></select></label>
<p><button name="decision" value="allow">Allow</button> <button name="decision" value="deny">Deny</button></p></form></html>`))

const csrfCookie = "__Host-baf-allow"

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	peer, err := s.identity.WhoIs(ctx, r.RemoteAddr)
	cancel()
	if err != nil || len(peer.Tags) > 0 || peer.Login == "" || peer.Login == "tagged-devices" {
		http.Error(w, "approve from a person's device on the tailnet; tagged devices cannot Allow", 403)
		return
	}
	if r.Method == "POST" {
		s.allow(w, r, peer.Login, peer.Node)
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || !singleValues(q) {
		http.Error(w, "invalid authorization query", 400)
		return
	}
	name, ok := s.resourceAgent(q.Get("resource"))
	if !ok {
		http.Error(w, "resource must be this Relay's /door/vendor/name/mcp URL", 400)
		return
	}
	if err := s.allowed(name); err != nil {
		http.Error(w, err.Error(), 403)
		return
	}
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || !validChallenge(q.Get("code_challenge")) {
		http.Error(w, "need response_type=code and a valid S256 PKCE challenge", 400)
		return
	}
	scope := q.Get("scope")
	if scope == "" {
		scope = "baf"
	}
	if scope != "baf" {
		http.Error(w, "only the baf scope is supported", 400)
		return
	}
	c, err := s.lookupClient(r.Context(), q.Get("client_id"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if !slices.Contains(c.Redirects, q.Get("redirect_uri")) {
		http.Error(w, "redirect_uri is not registered for this client", 400)
		return
	}
	csrf := randomToken()
	a := approval{Grant: grant{Connector: Connector{Agent: name, ApprovedBy: peer.Login, ClientID: c.ID}, Resource: s.resource(name), Redirect: q.Get("redirect_uri"), Challenge: q.Get("code_challenge"), Scope: scope}, State: q.Get("state"), Node: peer.Node, Expires: s.now().Add(10 * time.Minute)}
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
		http.Error(w, "too many pending sign-ins; try again shortly", 429)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: csrf, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 600})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	allowPage.Execute(w, map[string]any{"Agent": name, "Login": peer.Login, "Client": c, "Redirect": a.Grant.Redirect, "Warn": !vendorMatches(name, c, a.Grant.Redirect), "CSRF": csrf})
}

func (s *Server) allow(w http.ResponseWriter, r *http.Request, login, node string) {
	if err := r.ParseForm(); err != nil || !singleValues(r.PostForm) {
		http.Error(w, "invalid Allow form", 400)
		return
	}
	q := r.PostForm
	csrf := q.Get("csrf")
	cookie, err := r.Cookie(csrfCookie)
	if err != nil || csrf == "" || subtle.ConstantTimeCompare([]byte(csrf), []byte(cookie.Value)) != 1 {
		http.Error(w, "invalid or missing Allow form guard; open the sign-in page again", 403)
		return
	}
	key := string(hash(csrf))
	s.mu.Lock()
	a, ok := s.pending[key]
	if ok && a.Grant.ApprovedBy == login && a.Node == node {
		delete(s.pending, key)
	} else {
		ok = false
	}
	s.mu.Unlock()
	if !ok || !s.now().Before(a.Expires) {
		http.Error(w, "sign-in expired or belongs to another device; open it again", 403)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	if err := s.allowed(a.Grant.Agent); err != nil {
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
	case "allow":
		days, err := strconv.Atoi(q.Get("days"))
		if err != nil || !slices.Contains([]int{7, 30, 90, 365}, days) {
			http.Error(w, "choose 7, 30, 90 or 365 days", 400)
			return
		}
		a.Grant.ApprovedAt = s.now()
		a.Grant.ExpiresAt = s.now().Add(time.Duration(days) * 24 * time.Hour)
		code, err := s.approve(a.Grant)
		if err != nil {
			http.Error(w, "could not save connector", 500)
			return
		}
		values.Set("code", code)
	default:
		http.Error(w, "choose Allow or Deny", 400)
		return
	}
	back.RawQuery = values.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}
