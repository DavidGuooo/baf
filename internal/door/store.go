package door

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

// Connector is the local record of one sign-in. It never leaves the local API.
type Connector struct {
	Agent      string    `json:"agent"`
	Connected  bool      `json:"connected"`
	ApprovedBy string    `json:"approved_by,omitempty"`
	ApprovedAt time.Time `json:"approved_at,omitempty"`
	ClientID   string    `json:"client_id,omitempty"`
	State      string    `json:"state"`
}

type grant struct {
	Connector
	Resource, Redirect, Challenge, Scope string
}

type tokens struct {
	Access    string `json:"access_token"`
	Refresh   string `json:"refresh_token"`
	Type      string `json:"token_type"`
	ExpiresIn int64  `json:"expires_in"`
	Scope     string `json:"scope"`
}

var errGrant = errors.New("invalid_grant")

const schema = `
CREATE TABLE IF NOT EXISTS connectors (
 agent TEXT PRIMARY KEY,
 approved_by TEXT NOT NULL,
 approved_at INTEGER NOT NULL,
 client_id TEXT NOT NULL,
 expires_at INTEGER NOT NULL,
 resource TEXT NOT NULL,
 redirect TEXT NOT NULL,
 challenge TEXT NOT NULL,
 scope TEXT NOT NULL,
 code_hash BLOB UNIQUE,
 code_expires INTEGER NOT NULL,
 refresh_hash BLOB UNIQUE
);
CREATE TABLE IF NOT EXISTS access_tokens (
 hash BLOB PRIMARY KEY,
 agent TEXT NOT NULL REFERENCES connectors(agent) ON DELETE CASCADE,
 expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS access_tokens_by_agent ON access_tokens(agent);
CREATE TABLE IF NOT EXISTS clients (
 id TEXT PRIMARY KEY,
 metadata TEXT NOT NULL,
 created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS subscriptions (
 id TEXT PRIMARY KEY,
 agent TEXT NOT NULL REFERENCES connectors(agent) ON DELETE CASCADE,
 url TEXT NOT NULL,
 secret TEXT NOT NULL,
 generation TEXT NOT NULL,
 after_seq INTEGER NOT NULL,
 expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS event_deliveries (
 id TEXT PRIMARY KEY,
 subscription TEXT NOT NULL REFERENCES subscriptions(id) ON DELETE CASCADE,
 body BLOB NOT NULL,
 attempts INTEGER NOT NULL DEFAULT 0,
 next_try_at INTEGER NOT NULL,
 expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS events_due ON event_deliveries(next_try_at);
CREATE TABLE IF NOT EXISTS event_position (
 id INTEGER PRIMARY KEY CHECK (id = 1),
 seq INTEGER NOT NULL
);
INSERT OR IGNORE INTO event_position VALUES (1, 0);`

func openDB(path string) (*sql.DB, error) {
	// Create with owner-only permissions even when baf's home already exists.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	q := url.Values{
		"_pragma": {"journal_mode(WAL)", "busy_timeout(5000)", "synchronous(FULL)", "foreign_keys(1)"},
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func randomToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hash(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

func challenge(verifier string) string { return base64.RawURLEncoding.EncodeToString(hash(verifier)) }

// approve replaces the connector immediately, including any unspent code.
// The old connector expires_at column is kept for existing databases. New
// approvals store zero; no operation reads it. Tokens keep their own expiry.
func (s *Server) approve(g grant) (string, error) {
	code := randomToken()
	_, err := s.db.Exec(`INSERT OR REPLACE INTO connectors
 (agent,approved_by,approved_at,client_id,expires_at,resource,redirect,challenge,scope,code_hash,code_expires)
 VALUES (?,?,?,?,?,?,?,?,?,?,?)`, g.Agent, g.ApprovedBy, g.ApprovedAt.Unix(), g.ClientID,
		0, g.Resource, g.Redirect, g.Challenge, g.Scope,
		hash(code), s.now().Add(5*time.Minute).Unix())
	return code, err
}

// exchange consumes a code or rotates a refresh token in the same transaction
// that saves its replacement. Two concurrent uses cannot both succeed.
func (s *Server) exchange(form url.Values) (out tokens, err error) {
	kind := form.Get("grant_type")
	var g grant
	defer func() {
		if err == nil {
			slog.Info("door token issued", "agent", g.Agent, "grant_type", kind)
			return
		}
		attrs := []any{"agent", g.Agent, "reason", refusalReason(err)}
		if kind == "authorization_code" || kind == "refresh_token" {
			attrs = append(attrs, "grant_type", kind)
		}
		if refusalReason(err) == "resource mismatch" {
			attrs = append(attrs, "expected_resource", logURL(g.Resource), "given_resource", logURL(form.Get("resource")))
		}
		slog.Info("door token refused", attrs...)
	}()
	if kind != "authorization_code" && kind != "refresh_token" {
		return tokens{}, grantRefusal("unsupported grant type")
	}
	column, raw := "code_hash", form.Get("code")
	if kind == "refresh_token" {
		column, raw = "refresh_hash", form.Get("refresh_token")
	}
	unknown := "unknown or used code"
	if kind == "refresh_token" {
		unknown = "unknown or used refresh token"
	}
	if raw == "" {
		return tokens{}, grantRefusal(unknown)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return tokens{}, err
	}
	defer tx.Rollback()
	var codeExpires int64
	err = tx.QueryRow(`SELECT agent,client_id,resource,redirect,challenge,scope,code_expires
 FROM connectors WHERE `+column+`=?`, hash(raw)).Scan(
		&g.Agent, &g.ClientID, &g.Resource, &g.Redirect, &g.Challenge, &g.Scope, &codeExpires)
	if errors.Is(err, sql.ErrNoRows) {
		return tokens{}, grantRefusal(unknown)
	}
	if err != nil {
		return tokens{}, err
	}
	now := s.now()
	if reason := exchangeRefusal(g, form, codeExpires, now); reason != "" {
		return tokens{}, grantRefusal(reason)
	}
	if err := s.allowed(g.Agent); err != nil {
		return tokens{}, policyRefusal(err, "name no longer in the policy")
	}
	until := now.Add(24 * time.Hour).Unix()
	out = tokens{
		Access: randomToken(), Refresh: randomToken(), Type: "Bearer",
		ExpiresIn: until - now.Unix(), Scope: g.Scope,
	}
	_, err = tx.Exec(`UPDATE connectors SET code_hash=NULL, refresh_hash=? WHERE agent=?`,
		hash(out.Refresh), g.Agent)
	if err != nil {
		return tokens{}, err
	}
	_, err = tx.Exec(`DELETE FROM access_tokens WHERE expires_at<=?`, now.Unix())
	if err != nil {
		return tokens{}, err
	}
	_, err = tx.Exec(`INSERT INTO access_tokens VALUES (?,?,?)`, hash(out.Access), g.Agent, until)
	if err != nil {
		return tokens{}, err
	}
	if err = tx.Commit(); err != nil {
		return tokens{}, err
	}
	return out, nil
}

// Authenticate binds a bearer token to exactly one Door agent and resource.
// Use the returned name as the Principal, never a request-body name.
func (s *Server) Authenticate(token, resource string) (string, error) {
	var name, expectedResource string
	var accessExpires int64
	err := s.db.QueryRow(`SELECT c.agent,c.resource,a.expires_at
 FROM access_tokens a JOIN connectors c ON a.agent=c.agent
 WHERE a.hash=?`, hash(token)).Scan(&name, &expectedResource, &accessExpires)
	if errors.Is(err, sql.ErrNoRows) {
		return "", grantRefusal("unknown")
	}
	if err != nil {
		return "", grantRefusal("store unavailable")
	}
	if resource != expectedResource {
		return "", grantRefusal("wrong resource")
	}
	if s.now().Unix() >= accessExpires {
		return "", grantRefusal("expired")
	}
	if err := s.allowed(name); err != nil {
		return "", policyRefusal(err, "not in policy")
	}
	return name, nil
}

// List includes every name in the current policy, even without a connector.
func (s *Server) List() ([]Connector, error) {
	names, err := s.policy.DoorAgents()
	if err != nil {
		return nil, err
	}
	out := make([]Connector, 0, len(names))
	for _, name := range names {
		c := Connector{Agent: name, State: "none"}
		var approved, codeExpires int64
		var pending bool
		var redirect string
		err := s.db.QueryRow(`SELECT approved_by,approved_at,client_id,refresh_hash IS NOT NULL,code_hash IS NOT NULL,code_expires,redirect
 FROM connectors WHERE agent=?`, name).Scan(&c.ApprovedBy, &approved, &c.ClientID, &c.Connected, &pending, &codeExpires, &redirect)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			c.ApprovedAt = time.Unix(approved, 0).UTC()
			switch {
			case c.Connected:
				c.State = "connected"
			case pending && s.now().Unix() < codeExpires:
				c.State = "waiting for the app"
				if openAIURL(redirect) && (!httpsURL(c.ClientID) || openAIURL(c.ClientID)) {
					c.State = "waiting for ChatGPT"
				}
			case pending:
				c.State = "sign-in expired"
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// Revoke also deletes an outstanding code, including for a name removed from
// the policy. It is safe to repeat.
func (s *Server) Revoke(name string) error {
	if !validAgent(name) {
		return errors.New("Door agent must be vendor/name")
	}
	_, err := s.db.Exec(`DELETE FROM connectors WHERE agent=?`, name)
	return err
}
