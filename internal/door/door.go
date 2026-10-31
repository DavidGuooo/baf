// Package door serves the Relay's Public door and its tailnet-only sign-in.
// Permissions always come from the mailbox's current Tailscale policy lookup.
package door

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/DavidGuooo/baf/internal/mailbox"
	"github.com/DavidGuooo/baf/internal/tailnet"
)

const maxRequest = 16 << 10

// Policy is the mailbox's lookup of Door agents in this Relay's own grant.
type Policy interface {
	DoorAgents() ([]string, error)
}

// Identity asks about the actual connection, never forwarded HTTP headers.
type Identity interface {
	WhoIs(context.Context, string) (tailnet.Peer, error)
}

// Options supplies storage, URLs and the current Tailscale lookups.
type Options struct {
	Path      string
	PublicURL string
	SigninURL string
	Policy    Policy
	Identity  Identity
	Mailbox   *mailbox.Mailbox // required for MCP tools
}

// Server serves sign-in and authenticated access for Door agents.
type Server struct {
	db                   *sql.DB
	publicURL, signinURL string
	policy               Policy
	identity             Identity
	mailbox              *mailbox.Mailbox
	writes               map[string][]time.Time
	fetch                *http.Client
	now                  func() time.Time
	mu                   sync.Mutex
	pending              map[string]approval
}

// Open opens the connector store without starting any listeners.
func Open(o Options) (*Server, error) {
	if o.Policy == nil || o.Identity == nil {
		return nil, errors.New("door needs policy and Tailscale identity lookups")
	}
	if !httpsURL(o.PublicURL) || !httpsURL(o.SigninURL) {
		return nil, errors.New("door needs HTTPS public and sign-in URLs")
	}
	db, err := openDB(o.Path)
	if err != nil {
		return nil, err
	}
	return &Server{
		db:        db,
		publicURL: strings.TrimRight(o.PublicURL, "/"), signinURL: strings.TrimRight(o.SigninURL, "/"),
		policy: o.Policy, identity: o.Identity, mailbox: o.Mailbox,
		writes: map[string][]time.Time{},
		fetch:  metadataClient(), now: time.Now, pending: map[string]approval{},
	}, nil
}

// Close closes the connector store and idle metadata connections.
func (s *Server) Close() error {
	s.fetch.CloseIdleConnections()
	return s.db.Close()
}

func validAgent(name string) bool {
	_, err := envelope.ParseAddress(name)
	return err == nil
}

func (s *Server) allowed(name string) error {
	names, err := s.policy.DoorAgents()
	if err != nil {
		return err
	}
	if !slices.Contains(names, name) {
		return fmt.Errorf("the Tailscale policy does not list %q as a Door agent on this Relay", name)
	}
	return nil
}

func agentPath(path string) (string, bool) {
	parts := strings.Split(path, "/")
	if len(parts) != 5 || parts[0] != "" || parts[1] != "door" || parts[4] != "mcp" {
		return "", false
	}
	name := parts[2] + "/" + parts[3]
	return name, validAgent(name)
}

func (s *Server) resource(name string) string { return s.publicURL + "/door/" + name + "/mcp" }

func (s *Server) resourceAgent(resource string) (string, bool) {
	path, ok := strings.CutPrefix(resource, s.publicURL)
	if !ok {
		return "", false
	}
	name, ok := agentPath(path)
	return name, ok && resource == s.resource(name)
}

// PublicHandler deliberately does not use ServeMux's path redirects. Unknown
// paths, including scanner requests, get only a 404 with an empty body.
func (s *Server) PublicHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.EscapedPath()
		if r.Method == "GET" && path == "/.well-known/oauth-authorization-server" {
			if !limitRequest(w, r) {
				return
			}
			s.authServer(w)
			return
		}
		if r.Method == "POST" && (path == "/token" || path == "/register") {
			if !limitRequest(w, r) {
				return
			}
			if path == "/token" {
				s.token(w, r)
			} else {
				s.register(w, r)
			}
			return
		}
		if p, ok := strings.CutPrefix(path, "/.well-known/oauth-protected-resource"); ok && r.Method == "GET" {
			if name, ok := agentPath(p); ok {
				if !limitRequest(w, r) {
					return
				}
				writeJSON(w, 200, map[string]any{
					"resource": s.resource(name), "authorization_servers": []string{s.publicURL},
					"scopes_supported": []string{"baf"}, "bearer_methods_supported": []string{"header"},
					"resource_name": "baf: " + name,
				})
				return
			}
		}
		if name, ok := agentPath(path); ok && (r.Method == "POST" || r.Method == "GET" || r.Method == "DELETE") {
			if !limitBody(w, r, maxMCPRequest) {
				return
			}
			auth := strings.Fields(r.Header.Get("Authorization"))
			if len(auth) == 2 && strings.EqualFold(auth[0], "Bearer") {
				if principal, err := s.Authenticate(auth[1], s.resource(name)); err == nil {
					s.mcp(w, r, principal)
					return
				}
			}
			metadata := s.publicURL + "/.well-known/oauth-protected-resource/door/" + name + "/mcp"
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s"`, metadata))
			oauthError(w, 401, "invalid_token")
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
}

// SigninHandler serves the Allow page on the tailnet-only listener.
func (s *Server) SigninHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/authorize" || (r.Method != "GET" && r.Method != "POST") {
			w.WriteHeader(404)
			return
		}
		if !limitRequest(w, r) {
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; "+
			"form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		s.authorize(w, r)
	})
}

func (s *Server) authServer(w http.ResponseWriter) {
	writeJSON(w, 200, map[string]any{
		"issuer": s.publicURL, "authorization_endpoint": s.signinURL + "/authorize",
		"token_endpoint": s.publicURL + "/token", "registration_endpoint": s.publicURL + "/register",
		"authorization_response_iss_parameter_supported": true, "client_id_metadata_document_supported": true,
		"token_endpoint_auth_methods_supported": []string{"none"},
		"code_challenge_methods_supported":      []string{"S256"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"response_types_supported":              []string{"code"}, "scopes_supported": []string{"baf"},
	})
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	if err := r.ParseForm(); err != nil {
		oauthError(w, 400, "invalid_request")
		return
	}
	q := r.PostForm
	if !singleValues(q) {
		oauthError(w, 400, "invalid_request")
		return
	}
	if q.Get("grant_type") != "authorization_code" && q.Get("grant_type") != "refresh_token" {
		oauthError(w, 400, "unsupported_grant_type")
		return
	}
	out, err := s.exchange(q)
	if errors.Is(err, errGrant) {
		oauthError(w, 400, "invalid_grant")
		return
	}
	if err != nil {
		oauthError(w, 500, "server_error")
		return
	}
	writeJSON(w, 200, out)
}

func validVerifier(v string) bool {
	if len(v) < 43 || len(v) > 128 {
		return false
	}
	for _, ch := range v {
		if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~", ch) {
			return false
		}
	}
	return true
}

func validChallenge(v string) bool {
	b, err := base64.RawURLEncoding.DecodeString(v)
	return err == nil && len(b) == 32 && base64.RawURLEncoding.EncodeToString(b) == v
}

func singleValues(q url.Values) bool {
	for _, values := range q {
		if len(values) != 1 {
			return false
		}
	}
	return true
}

func limitRequest(w http.ResponseWriter, r *http.Request) bool {
	return limitBody(w, r, maxRequest)
}

func limitBody(w http.ResponseWriter, r *http.Request, limit int64) bool {
	if len(r.RequestURI) > maxRequest || r.ContentLength > limit {
		w.WriteHeader(413)
		return false
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		w.WriteHeader(413)
		return false
	}
	r.Body = io.NopCloser(strings.NewReader(string(data)))
	return true
}

func oauthError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
