package door

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/DavidGuooo/baf/internal/envelope"
)

const eventName = "message.created"
const defaultSubscriptionTTL = 24 * time.Hour
const maxSubscriptionTTL = 7 * 24 * time.Hour

var mcpEvents = []map[string]any{{
	"name": eventName,
	"description": "New mail for you, including Results. Carries message ids, sender, recipient and kind. " +
		"Call read with id to open the message, then act; use reply for a Request. " +
		"Delivery does not mark it read. Events may repeat; use id to avoid answering twice. Check inbox for missed mail.",
	"delivery":    []string{"webhook"},
	"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
	"payloadSchema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":       stringProperty("Message id; call read with this id to open it."),
			"from":     stringProperty("The stored Principal: node/agent."),
			"to":       stringProperty("Your Door agent address."),
			"kind":     map[string]any{"type": "string", "enum": []string{"notice", "request", "result"}},
			"reply_to": stringProperty("The Request id this Result answers, when set."),
		},
		"required":             []string{"id", "from", "to", "kind"},
		"additionalProperties": false,
	},
}}

type eventMessage struct {
	ID      string        `json:"id"`
	From    string        `json:"from"`
	To      string        `json:"to"`
	Kind    envelope.Kind `json:"kind"`
	ReplyTo string        `json:"reply_to,omitempty"`
}

type event struct {
	ID        string       `json:"eventId"`
	Name      string       `json:"name"`
	Timestamp time.Time    `json:"timestamp"`
	Data      eventMessage `json:"data"`
	Cursor    *string      `json:"cursor"`
}

type delivery struct {
	Mode   string `json:"mode"`
	URL    string `json:"url"`
	Secret string `json:"secret"`
}

type subParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Delivery  delivery        `json:"delivery"`
	TTL       json.RawMessage `json:"ttlMs"`
}

func parseSubscription(raw json.RawMessage) (subParams, error) {
	var p subParams
	if err := json.Unmarshal(raw, &p); err != nil || p.Name != eventName {
		return p, errors.New("name must be message.created")
	}
	var args map[string]any
	if len(p.Arguments) != 0 && (json.Unmarshal(p.Arguments, &args) != nil || len(args) != 0) {
		return p, errors.New("message.created takes no arguments")
	}
	if p.Delivery.Mode != "webhook" || !httpsURL(p.Delivery.URL) || len(p.Delivery.URL) > 4096 {
		return p, errors.New("delivery must be webhook with an HTTPS callback URL")
	}
	return p, nil
}

// Match the probe's id, including its canonical empty arguments object.
func subID(principal, url string) string {
	h := sha256.Sum256([]byte(principal + "\n" + url + "\n" + eventName + "\n{}"))
	return "sub_" + hex.EncodeToString(h[:12])
}

func (s *Server) subscribe(ctx context.Context, principal string, raw json.RawMessage) (any, *rpcError) {
	p, err := parseSubscription(raw)
	if err != nil {
		return nil, &rpcError{-32602, err.Error()}
	}
	if _, err := secretKey(p.Delivery.Secret); err != nil {
		return nil, &rpcError{-32602, err.Error()}
	}
	ttl := defaultSubscriptionTTL
	if string(p.TTL) == "null" {
		ttl = maxSubscriptionTTL
	} else if len(p.TTL) != 0 {
		var ms int64
		if json.Unmarshal(p.TTL, &ms) != nil || ms <= 0 {
			return nil, &rpcError{-32602, "ttlMs must be positive milliseconds or null"}
		}
		// Clamp before converting, so a very large duration cannot overflow.
		ttl = time.Duration(max(int64(time.Minute/time.Millisecond), min(ms, maxSubscriptionTTL.Milliseconds()))) * time.Millisecond
	}
	if s.mailbox == nil {
		return nil, &rpcError{-32603, "mailbox is unavailable"}
	}
	// Remember this connector while verification is in flight. Revoke, a new
	// sign-in or token rotation must not let an old challenge add a subscription.
	var connector []byte
	if err := s.db.QueryRow(`SELECT refresh_hash FROM connectors WHERE agent=?`,
		principal).Scan(&connector); err != nil || len(connector) == 0 {
		return nil, &rpcError{-32602, "connector is no longer active"}
	}
	id := subID(principal, p.Delivery.URL)
	if err := s.verifyCallback(ctx, id, p.Delivery); err != nil {
		slog.Info("door events verification failed", "agent", principal, "subscription", id)
		return nil, &rpcError{-32015, "callback verification failed: " + err.Error()}
	}
	seq, err := s.mailbox.DoorSequence()
	if err != nil {
		return nil, &rpcError{-32603, "could not read mailbox position"}
	}
	expires := s.now().Add(ttl).UTC()
	// Renew in place, keeping the original boundary and queued events. An
	// expired subscription starts at today's boundary instead of replaying mail.
	tx, err := s.db.Begin()
	if err != nil {
		return nil, &rpcError{-32603, "could not store subscription"}
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM subscriptions WHERE id=? AND expires_at<=?`, id, s.now().UnixMilli()); err != nil {
		return nil, &rpcError{-32603, "could not expire subscription"}
	}
	result, err := tx.Exec(`INSERT INTO subscriptions (id,agent,url,secret,generation,after_seq,expires_at)
 SELECT ?,agent,?,?,?,?,? FROM connectors
 WHERE agent=? AND refresh_hash=?
 ON CONFLICT(id) DO UPDATE SET secret=excluded.secret,generation=excluded.generation,
 expires_at=excluded.expires_at`, id, p.Delivery.URL, p.Delivery.Secret, randomToken(), seq, expires.UnixMilli(),
		principal, connector)
	if err != nil {
		return nil, &rpcError{-32603, "could not store subscription"}
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return nil, &rpcError{-32602, "connector changed; subscribe again"}
	}
	var until int64
	if err := tx.QueryRow(`SELECT expires_at FROM subscriptions WHERE id=?`, id).Scan(&until); err != nil {
		return nil, &rpcError{-32602, "subscription was removed"}
	}
	if err := tx.Commit(); err != nil {
		return nil, &rpcError{-32603, "could not store subscription"}
	}
	expires = time.UnixMilli(until).UTC()
	slog.Info("door events subscribed", "agent", principal, "subscription", id, "until", expires)
	s.Wake()
	return map[string]any{"resultType": "complete", "id": id, "refreshBefore": expires.Format(time.RFC3339),
		"cursor": nil, "truncated": false}, nil
}

func (s *Server) unsubscribe(principal string, raw json.RawMessage) (any, *rpcError) {
	p, err := parseSubscription(raw)
	if err != nil {
		return nil, &rpcError{-32602, err.Error()}
	}
	id := subID(principal, p.Delivery.URL)
	if _, err := s.db.Exec(`DELETE FROM subscriptions WHERE id=? AND agent=?`, id, principal); err != nil {
		return nil, &rpcError{-32603, "could not remove subscription"}
	}
	slog.Info("door events unsubscribed", "agent", principal, "subscription", id)
	return map[string]any{"resultType": "complete"}, nil
}

func secretKey(secret string) ([]byte, error) {
	if !strings.HasPrefix(secret, "whsec_") {
		return nil, errors.New("delivery.secret must start with whsec_")
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil || len(key) < 24 || len(key) > 64 {
		return nil, errors.New("delivery.secret must be base64 encoding of 24-64 bytes")
	}
	return key, nil
}

// sign uses the Standard Webhooks format, as in the tested probe.
func sign(key []byte, id string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%s.%d.", id, timestamp)
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func callbackClient() *http.Client {
	// Use the same dial-time public-address checks as client metadata: no
	// proxy, loopback, private network, tailnet addresses or DNS rebinding.
	c := metadataClient()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

func (s *Server) postEvent(ctx context.Context, url, id, subscription, secret string, body []byte) (int, []byte, error) {
	if !httpsURL(url) {
		return 0, nil, errors.New("callback must be HTTPS")
	}
	key, err := secretKey(secret)
	if err != nil {
		return 0, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, errors.New("invalid callback URL")
	}
	ts := s.now().Unix()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("webhook-id", id)
	req.Header.Set("webhook-timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("webhook-signature", sign(key, id, ts, body))
	req.Header.Set("X-MCP-Subscription-Id", subscription)
	resp, err := s.callbacks.Do(req)
	if err != nil {
		// URLs may contain callback credentials; never return or log net/http's error.
		return 0, nil, errors.New("callback unreachable or timed out")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return resp.StatusCode, nil, errors.New("callback response too large or unreadable")
	}
	return resp.StatusCode, data, nil
}

func (s *Server) verifyCallback(ctx context.Context, id string, d delivery) error {
	challenge := randomToken()
	body, _ := json.Marshal(map[string]string{"type": "verification", "challenge": challenge})
	status, data, err := s.postEvent(ctx, d.URL, "msg_verification_"+randomToken(), id, d.Secret, body)
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return fmt.Errorf("HTTP %d", status)
	}
	var answer struct {
		Challenge string `json:"challenge"`
	}
	if json.Unmarshal(data, &answer) != nil || !hmac.Equal([]byte(answer.Challenge), []byte(challenge)) {
		return errors.New("challenge not echoed")
	}
	return nil
}
