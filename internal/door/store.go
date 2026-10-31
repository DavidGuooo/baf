package door

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
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
	ExpiresAt  time.Time `json:"expires_at,omitempty"`
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
);`

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
func (s *Server) approve(g grant) (string, error) {
	code := randomToken()
	_, err := s.db.Exec(`INSERT OR REPLACE INTO connectors
 (agent,approved_by,approved_at,client_id,expires_at,resource,redirect,challenge,scope,code_hash,code_expires)
 VALUES (?,?,?,?,?,?,?,?,?,?,?)`, g.Agent, g.ApprovedBy, g.ApprovedAt.Unix(), g.ClientID,
		g.ExpiresAt.Unix(), g.Resource, g.Redirect, g.Challenge, g.Scope,
		hash(code), s.now().Add(5*time.Minute).Unix())
	return code, err
}

// exchange consumes a code or rotates a refresh token in the same transaction
// that saves its replacement. Two concurrent uses cannot both succeed.
func (s *Server) exchange(form url.Values) (tokens, error) {
	kind := form.Get("grant_type")
	column, raw := "code_hash", form.Get("code")
	if kind == "refresh_token" {
		column, raw = "refresh_hash", form.Get("refresh_token")
	}
	if raw == "" {
		return tokens{}, errGrant
	}
	tx, err := s.db.Begin()
	if err != nil {
		return tokens{}, err
	}
	defer tx.Rollback()
	var g grant
	var expires, codeExpires int64
	err = tx.QueryRow(`SELECT agent,client_id,expires_at,resource,redirect,challenge,scope,code_expires
 FROM connectors WHERE `+column+`=?`, hash(raw)).Scan(
		&g.Agent, &g.ClientID, &expires, &g.Resource, &g.Redirect, &g.Challenge, &g.Scope, &codeExpires)
	if errors.Is(err, sql.ErrNoRows) {
		return tokens{}, errGrant
	}
	if err != nil {
		return tokens{}, err
	}
	now := s.now()
	// A code or refresh token is valid only for its approved client and resource.
	if now.Unix() >= expires || form.Get("client_id") != g.ClientID ||
		(form.Get("resource") != "" && form.Get("resource") != g.Resource) {
		return tokens{}, errGrant
	}
	if kind == "authorization_code" && (now.Unix() >= codeExpires || form.Get("redirect_uri") != g.Redirect ||
		!validVerifier(form.Get("code_verifier")) || challenge(form.Get("code_verifier")) != g.Challenge) {
		return tokens{}, errGrant
	}
	if err := s.allowed(g.Agent); err != nil {
		return tokens{}, errGrant
	}
	until := min(now.Add(24*time.Hour).Unix(), expires)
	out := tokens{
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
	var name string
	var expires, accessExpires int64
	err := s.db.QueryRow(`SELECT c.agent,c.expires_at,a.expires_at
 FROM access_tokens a JOIN connectors c ON a.agent=c.agent
 WHERE a.hash=? AND c.resource=?`, hash(token), resource).Scan(&name, &expires, &accessExpires)
	if err != nil {
		return "", errGrant
	}
	if s.now().Unix() >= min(expires, accessExpires) {
		return "", errGrant
	}
	if err := s.allowed(name); err != nil {
		return "", errGrant
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
		c := Connector{Agent: name}
		var approved, expires int64
		var active bool
		err := s.db.QueryRow(`SELECT approved_by,approved_at,client_id,expires_at,refresh_hash IS NOT NULL
 FROM connectors WHERE agent=?`, name).Scan(&c.ApprovedBy, &approved, &c.ClientID, &expires, &active)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			c.ApprovedAt, c.ExpiresAt = time.Unix(approved, 0).UTC(), time.Unix(expires, 0).UTC()
			c.Connected = active && s.now().Unix() < expires
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
