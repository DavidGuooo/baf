// Package transport moves messages between mailboxes on different nodes
// over the tailnet: plain HTTP, which WireGuard already encrypts and
// authenticates. The receiving side never trusts the message for who sent
// it; it asks Tailscale.
package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/DavidGuooo/baf/internal/mailbox"
	"github.com/DavidGuooo/baf/internal/tailnet"
)

// DefaultPort is baf's one port on every node. (Yobiru uses 7766; ADR-0001.)
const DefaultPort = 7780

// Identity answers "who is connecting from this address, with what baf
// permission". In production that is Tailscale's WhoIs; tests use a fake.
type Identity interface {
	WhoIs(ctx context.Context, remoteAddr string) (tailnet.Peer, error)
}

// pushResponse is the body of every answer to a push.
type pushResponse struct {
	Receipt  *mailbox.Receipt `json:"receipt,omitempty"`
	Error    string           `json:"error,omitempty"`
	Rejected bool             `json:"rejected,omitempty"` // final: don't retry
}

type fileResponse struct {
	Stored   bool   `json:"stored,omitempty"`
	Error    string `json:"error,omitempty"`
	Rejected bool   `json:"rejected,omitempty"`
}

// progressResponse is the body of every answer to a progress question.
type progressResponse struct {
	Progress *mailbox.Progress `json:"progress,omitempty"`
	Error    string            `json:"error,omitempty"`
}

// Handler serves POST /v1/push, where other mailboxes hand over messages,
// PUT /v1/files/{sha256}, and GET /v1/progress/{id}.
func Handler(mb *mailbox.Mailbox, id Identity) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/push", func(w http.ResponseWriter, r *http.Request) {
		peer, err := id.WhoIs(r.Context(), r.RemoteAddr)
		if err != nil {
			// Not a tailnet peer Tailscale can name: say nothing useful.
			slog.Warn("push from unknown caller", "addr", r.RemoteAddr, "err", err)
			reply(w, http.StatusForbidden, pushResponse{Error: "unknown caller", Rejected: true})
			return
		}
		var env envelope.Envelope
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&env); err != nil {
			reply(w, http.StatusBadRequest, pushResponse{Error: "bad envelope: " + err.Error(), Rejected: true})
			return
		}
		// Printed so the first real run shows what Tailscale delivered (T4).
		slog.Info("push received", "from_node", peer.Node, "tags", peer.Tags, "login", peer.Login,
			"from_agent", env.FromAgent, "to", env.To, "kind", env.Kind, "id", env.ID, "grants", grantsJSON(peer.Grants))

		receipt, err := mb.AcceptPush(r.Context(), env, peer)
		var rej *mailbox.Rejection
		switch {
		case errors.As(err, &rej):
			reply(w, http.StatusUnprocessableEntity, pushResponse{Error: rej.Reason, Rejected: true})
		case err != nil:
			slog.Error("store pushed message", "id", env.ID, "err", err)
			reply(w, http.StatusInternalServerError, pushResponse{Error: "could not store the message"})
		default:
			reply(w, http.StatusOK, pushResponse{Receipt: &receipt})
		}
	})

	mux.HandleFunc("PUT /v1/files/{sha256}", func(w http.ResponseWriter, r *http.Request) {
		peer, err := id.WhoIs(r.Context(), r.RemoteAddr)
		if err != nil {
			reply(w, http.StatusForbidden, fileResponse{Error: "unknown caller", Rejected: true})
			return
		}
		// Older senders use ?to=agent; full addresses use ?address=node/agent.
		address := r.URL.Query().Get("address")
		if address == "" {
			address = mb.Node() + "/" + r.URL.Query().Get("to")
		}
		to, err := envelope.ParseAddress(address)
		if err != nil {
			reply(w, http.StatusBadRequest, fileResponse{Error: "bad to: " + err.Error(), Rejected: true})
			return
		}
		if _, err := mb.CheckIncoming(r.Context(), peer, to, r.URL.Query().Get("origin")); err != nil {
			var rej *mailbox.Rejection
			final := errors.As(err, &rej)
			code := http.StatusServiceUnavailable
			if final {
				code = http.StatusForbidden
			}
			reply(w, code, fileResponse{Error: err.Error(), Rejected: final})
			return
		}
		if r.ContentLength < 0 || (r.ContentLength == 0 && r.Header.Get("Content-Length") == "") {
			reply(w, http.StatusLengthRequired, fileResponse{Error: "file upload needs Content-Length", Rejected: true})
			return
		}
		if r.ContentLength > envelope.MaxFileSize {
			reply(w, http.StatusRequestEntityTooLarge, fileResponse{Error: "file exceeds the size limit", Rejected: true})
			return
		}
		f := envelope.File{Name: "upload", SHA256: r.PathValue("sha256"), Size: r.ContentLength}
		err = mb.StoreFile(f, http.MaxBytesReader(w, r.Body, f.Size+1))
		var rej *mailbox.Rejection
		switch {
		case errors.As(err, &rej):
			reply(w, http.StatusUnprocessableEntity, fileResponse{Error: rej.Reason, Rejected: true})
		case err != nil:
			slog.Error("store uploaded file", "sha256", f.SHA256, "err", err)
			reply(w, http.StatusInternalServerError, fileResponse{Error: "could not store the file"})
		default:
			slog.Info("file received", "from_node", peer.Node, "sha256", f.SHA256, "size", f.Size)
			reply(w, http.StatusOK, fileResponse{Stored: true})
		}
	})

	// Only the sending node, or the recorded forwarding Relay, may ask.
	// Its current policy must still allow the message (ADR-0006).
	mux.HandleFunc("GET /v1/progress/{id}", func(w http.ResponseWriter, r *http.Request) {
		peer, err := id.WhoIs(r.Context(), r.RemoteAddr)
		if err != nil {
			slog.Warn("progress question from unknown caller", "addr", r.RemoteAddr, "err", err)
			reply(w, http.StatusForbidden, progressResponse{Error: "unknown caller"})
			return
		}
		msgID := r.PathValue("id")
		var p mailbox.Progress
		origin := r.URL.Query().Get("origin")
		if origin == "" {
			p, err = mb.Progress(peer.Node, msgID)
		} else {
			p, err = mb.ForwardedProgress(peer.Node, origin, msgID)
		}
		switch {
		case errors.Is(err, mailbox.ErrNotFound):
			reply(w, http.StatusNotFound, progressResponse{
				Error: fmt.Sprintf("%s has no message %s from %s", mb.Node(), msgID, peer.Node)})
			return
		case err != nil:
			slog.Error("read progress", "id", msgID, "err", err)
			reply(w, http.StatusInternalServerError, progressResponse{Error: "could not read the message's progress"})
			return
		}
		to, _ := envelope.ParseAddress(p.To)
		if _, err := mb.CheckIncoming(r.Context(), peer, to, p.Origin); err != nil {
			reply(w, http.StatusForbidden, progressResponse{
				Error: fmt.Sprintf("%s may no longer message %s, so it can't follow its messages there: %s", peer.Node, p.To, err)})
			return
		}
		reply(w, http.StatusOK, progressResponse{Progress: &p})
	})
	return mux
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func grantsJSON(g []tailnet.Grant) string {
	b, _ := json.Marshal(g)
	return string(b)
}

// Lookup finds a node's Tailscale address and whether it is online.
type Lookup func(ctx context.Context, node string) (netip.Addr, bool, error)

// Client pushes messages to other nodes' mailboxes. It implements
// mailbox.Transport.
type Client struct {
	Lookup    Lookup
	FindRelay func(context.Context) (string, error)
	Port      int // the receiver's baf port; DefaultPort if 0
	HTTP      *http.Client

	relayMu    sync.Mutex
	relayNode  string
	relayErr   error
	relayUntil time.Time
}

// Relay caches discovery for a minute. A failed handover asks afresh.
func (c *Client) Relay(ctx context.Context, refresh bool) (string, error) {
	c.relayMu.Lock()
	defer c.relayMu.Unlock()
	if !refresh && time.Now().Before(c.relayUntil) {
		return c.relayNode, c.relayErr
	}
	if c.FindRelay == nil {
		return "", errors.New("no Relay found: Relay discovery is unavailable")
	}
	c.relayNode, c.relayErr = c.FindRelay(ctx)
	c.relayUntil = time.Now().Add(time.Minute)
	return c.relayNode, c.relayErr
}

func (c *Client) Known(ctx context.Context, node string) error {
	_, _, err := c.Lookup(ctx, node)
	return err
}

func (c *Client) Push(ctx context.Context, node string, env envelope.Envelope) (mailbox.Receipt, error) {
	addr, online, err := c.Lookup(ctx, node)
	if err != nil {
		return mailbox.Receipt{}, err
	}
	if !online {
		// Try anyway: Tailscale's view can lag behind a node that just woke.
		slog.Debug("pushing to a node Tailscale shows offline", "node", node)
	}
	body, err := json.Marshal(env)
	if err != nil {
		return mailbox.Receipt{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.url(addr, "/v1/push"), bytes.NewReader(body))
	if err != nil {
		return mailbox.Receipt{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return mailbox.Receipt{}, err
	}
	defer resp.Body.Close()
	var pr pushResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&pr); err != nil {
		return mailbox.Receipt{}, fmt.Errorf("%s answered %s with an unreadable body", node, resp.Status)
	}
	switch {
	case resp.StatusCode == http.StatusOK && pr.Receipt != nil:
		return *pr.Receipt, nil
	case pr.Rejected:
		return mailbox.Receipt{}, &mailbox.Rejection{Reason: pr.Error}
	default:
		return mailbox.Receipt{}, fmt.Errorf("%s answered %s: %s", node, resp.Status, pr.Error)
	}
}

func (c *Client) Progress(ctx context.Context, node, id, origin string) (mailbox.Progress, error) {
	addr, _, err := c.Lookup(ctx, node)
	if err != nil {
		return mailbox.Progress{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.url(addr, "/v1/progress/"+url.PathEscape(id)+"?origin="+url.QueryEscape(origin)), nil)
	if err != nil {
		return mailbox.Progress{}, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return mailbox.Progress{}, err
	}
	defer resp.Body.Close()
	var pr progressResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&pr); err != nil {
		if resp.StatusCode == http.StatusNotFound {
			// A baf from before progress answers with the server's plain 404.
			return mailbox.Progress{}, fmt.Errorf("%s runs an older baf that can't report progress; update it there", node)
		}
		return mailbox.Progress{}, fmt.Errorf("%s answered %s with an unreadable body", node, resp.Status)
	}
	switch {
	case resp.StatusCode == http.StatusOK && pr.Progress != nil:
		return *pr.Progress, nil
	case pr.Error != "":
		return mailbox.Progress{}, errors.New(pr.Error) // written to be read as it is
	default:
		return mailbox.Progress{}, fmt.Errorf("%s answered %s", node, resp.Status)
	}
}

func (c *Client) PutFile(ctx context.Context, node, to, origin string, f envelope.File, body io.Reader) error {
	addr, _, err := c.Lookup(ctx, node)
	if err != nil {
		return err
	}
	q := url.Values{}
	// Keep the old form for direct uploads to nodes that have not upgraded.
	address, err := envelope.ParseAddress(to)
	if err != nil {
		return err
	}
	if address.Node == node {
		q.Set("to", address.Agent)
	} else {
		q.Set("address", to)
	}
	if origin != "" {
		q.Set("origin", origin)
	}
	req, err := http.NewRequestWithContext(ctx, "PUT", c.url(addr, "/v1/files/"+f.SHA256+"?"+q.Encode()), body)
	if err != nil {
		return err
	}
	req.ContentLength = f.Size
	// A zero-length reader otherwise becomes an upload of unknown length.
	if f.Size == 0 {
		req.Body = http.NoBody
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var fr fileResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&fr); err != nil {
		if resp.StatusCode == http.StatusNotFound {
			return &mailbox.Rejection{Reason: fmt.Sprintf("%s runs an older baf that can't receive files; update it there", node)}
		}
		return fmt.Errorf("%s answered %s with an unreadable body", node, resp.Status)
	}
	switch {
	case resp.StatusCode == http.StatusOK && fr.Stored:
		return nil
	case fr.Rejected:
		return &mailbox.Rejection{Reason: fr.Error}
	default:
		return fmt.Errorf("%s answered %s: %s", node, resp.Status, fr.Error)
	}
}

// url is the address of path on the mailbox at addr.
func (c *Client) url(addr netip.Addr, path string) string {
	port := c.Port
	if port == 0 {
		port = DefaultPort
	}
	return "http://" + net.JoinHostPort(addr.String(), strconv.Itoa(port)) + path
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP == nil {
		return http.DefaultClient
	}
	return c.HTTP
}

// Bell is one node's doorbell: a webhook and the key its owner gave us.
type Bell struct {
	URL string
	Key string
}

// Doorbells rings the doorbells this node holds keys for. It implements
// mailbox.Doorbell. For Grok, the bell is Bridge's routine webhook: the JSON
// body reaches the routine, which hands each
// message id to the addressed bot.
type Doorbells struct {
	From  string          // this node's name, so the ring says who rang
	Bells map[string]Bell // by node name
	HTTP  *http.Client
}

// ringBody is what a ring sends. It names messages, never their content:
// the bot reads the message itself with `baf read`.
type ringBody struct {
	Baf      int        `json:"baf"`
	From     string     `json:"from"`
	Messages []ringItem `json:"messages"`
}

type ringItem struct {
	ID   string `json:"id"`
	From string `json:"from"` // node/agent, so Bridge can answer without opening the message
	To   string `json:"to"`
	Kind string `json:"kind"`
}

func (d *Doorbells) Ring(ctx context.Context, node string, env envelope.Envelope) error {
	bell, ok := d.Bells[node]
	if !ok {
		return mailbox.ErrNoDoorbell
	}
	from, sender := d.From, d.From+"/"+env.FromAgent
	if env.Origin != "" {
		from, sender = env.Origin, env.Origin
	}
	body, _ := json.Marshal(ringBody{Baf: envelope.Version, From: from,
		Messages: []ringItem{{ID: env.ID, From: sender, To: env.To, Kind: string(env.Kind)}}})
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", bell.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bell.Key)
	hc := d.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("doorbell for %s answered %s", node, resp.Status)
	}
	slog.Info("rang doorbell", "node", node, "message", env.ID)
	return nil
}
