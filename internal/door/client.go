package door

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"time"
)

type client struct {
	ID        string   `json:"client_id"`
	Name      string   `json:"client_name"`
	Redirects []string `json:"redirect_uris"`
}

// Client metadata is fetched only from public HTTPS addresses. Check the
// actual dial address too, so DNS changes cannot reach the Relay's network.
func metadataClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("client metadata redirects are not allowed")
		},
		Transport: &http.Transport{DialContext: (&net.Dialer{
			Timeout: 5 * time.Second,
			Control: func(_, addr string, _ syscall.RawConn) error {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					return err
				}
				ip, err := netip.ParseAddr(host)
				if err != nil || !publicIP(ip) {
					return errors.New("client metadata must use a public address")
				}
				return nil
			},
		}).DialContext,
			TLSHandshakeTimeout: 5 * time.Second, MaxResponseHeaderBytes: 16 << 10,
			IdleConnTimeout: 30 * time.Second,
		},
	}
}

func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() &&
		!netip.MustParsePrefix("100.64.0.0/10").Contains(ip) && !netip.MustParsePrefix("0.0.0.0/8").Contains(ip)
}

func httpsURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Fragment == ""
}

func validClient(c client) bool {
	if len(c.Name) > 256 || len(c.Redirects) == 0 || len(c.Redirects) > 20 {
		return false
	}
	for _, redirect := range c.Redirects {
		if len(redirect) > 4096 || !httpsURL(redirect) {
			return false
		}
	}
	return true
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	refuse := func(status int, code, reason string) {
		slog.Info("door register refused", "reason", reason)
		oauthError(w, status, code)
	}
	var c client
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil || !validClient(c) {
		refuse(400, "invalid_client_metadata", "invalid client metadata")
		return
	}
	c.ID = randomToken()
	raw, err := json.Marshal(c)
	if err != nil {
		refuse(500, "server_error", "store unavailable")
		return
	}
	// Bound anonymous registration storage. Old registrations with no connector
	// expire after 30 days; approved clients remain available for another sign-in.
	tx, err := s.db.Begin()
	if err != nil {
		refuse(500, "server_error", "store unavailable")
		return
	}
	defer tx.Rollback()
	_, err = tx.Exec(`DELETE FROM clients WHERE created_at < ?
 AND id NOT IN (SELECT client_id FROM connectors)`, s.now().Add(-30*24*time.Hour).Unix())
	if err != nil {
		refuse(500, "server_error", "store unavailable")
		return
	}
	var count int
	if err = tx.QueryRow(`SELECT count(*) FROM clients`).Scan(&count); err != nil {
		refuse(500, "server_error", "store unavailable")
		return
	}
	if count >= 1000 {
		refuse(429, "temporarily_unavailable", "registration limit reached")
		return
	}
	_, err = tx.Exec(`INSERT INTO clients VALUES (?,?,?)`, c.ID, string(raw), s.now().Unix())
	if err != nil {
		refuse(500, "server_error", "store unavailable")
		return
	}
	if err = tx.Commit(); err != nil {
		refuse(500, "server_error", "store unavailable")
		return
	}
	slog.Info("door register accepted", "client_id", c.ID)
	writeJSON(w, 201, map[string]any{
		"client_id": c.ID, "client_id_issued_at": s.now().Unix(), "client_name": c.Name,
		"redirect_uris": c.Redirects, "token_endpoint_auth_method": "none",
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
	})
}

func (s *Server) lookupClient(ctx context.Context, id string) (client, error) {
	var c client
	var raw string
	err := s.db.QueryRow(`SELECT metadata FROM clients WHERE id=?`, id).Scan(&raw)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &c)
		return c, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return c, err
	}
	u, err := url.Parse(id)
	if err != nil || !httpsURL(id) || (u.Port() != "" && u.Port() != "443") {
		return c, errors.New("unknown client_id; use registration or an HTTPS client metadata URL on port 443")
	}
	failed := func(reason string) {
		slog.Info("door CIMD metadata fetch failed", "client_id", logURL(id), "reason", reason)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, id, nil)
	if err != nil {
		failed("invalid metadata request")
		return c, errors.New("invalid metadata request")
	}
	resp, err := s.fetch.Do(req)
	if err != nil {
		reason := "metadata request failed"
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			reason = "metadata request timed out"
		}
		failed(reason)
		return c, errors.New("could not fetch client metadata")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Info("door CIMD metadata fetch failed", "client_id", logURL(id), "reason", "HTTP status", "status", resp.StatusCode)
		return c, errors.New("client metadata did not return HTTP 200")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRequest+1))
	if err != nil || len(data) > maxRequest {
		failed("metadata too large or unreadable")
		return c, errors.New("client metadata is too large or unreadable")
	}
	if err := json.Unmarshal(data, &c); err != nil || c.ID != id || !validClient(c) {
		failed("invalid metadata or mismatched client_id")
		return c, errors.New("invalid client metadata or mismatched client_id")
	}
	return c, nil
}

func openAIURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || !httpsURL(raw) {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return slices.Contains([]string{"chatgpt.com", "openai.com"}, host) ||
		strings.HasSuffix(host, ".chatgpt.com") || strings.HasSuffix(host, ".openai.com")
}

func vendorMatches(agent string, c client, redirect string) bool {
	vendor, _, _ := strings.Cut(agent, "/")
	// A registered client has an opaque id, so its redirect is the only hint.
	return vendor == "openai" && openAIURL(redirect) && (!httpsURL(c.ID) || openAIURL(c.ID))
}
