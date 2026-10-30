// Package transport moves messages between mailboxes on different nodes
// over the tailnet: plain HTTP, which WireGuard already encrypts and
// authenticates. The receiving side never trusts the message for who sent
// it; it asks Tailscale (design notes section 3).
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
	"strconv"
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

// Handler serves POST /v1/push, where other mailboxes hand over messages.
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
		to, err := envelope.ParseAddress(env.To)
		if err != nil {
			reply(w, http.StatusBadRequest, pushResponse{Error: "bad to: " + err.Error(), Rejected: true})
			return
		}
		// Printed so the first real run shows what Tailscale delivered (T4).
		slog.Info("push received", "from_node", peer.Node, "tags", peer.Tags, "login", peer.Login,
			"from_agent", env.FromAgent, "to", env.To, "kind", env.Kind, "id", env.ID, "grants", grantsJSON(peer.Grants))

		// Default deny: only a grant naming this agent (or "*") lets it in.
		// The relay's origin field is not trusted yet; there is no relay.
		if !peer.MayMessage(to.Agent) {
			reason := fmt.Sprintf("%s may not message %s: no %s grant with \"to\" naming %q in the Tailscale policy",
				peer.Node, env.To, tailnet.Capability, to.Agent)
			reply(w, http.StatusForbidden, pushResponse{Error: reason, Rejected: true})
			return
		}
		receipt, err := mb.Accept(env, mailbox.Principal{Node: peer.Node, Agent: env.FromAgent})
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
	return mux
}

func reply(w http.ResponseWriter, code int, v pushResponse) {
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
	Lookup Lookup
	Port   int // the receiver's baf port; DefaultPort if 0
	HTTP   *http.Client
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
	port := c.Port
	if port == 0 {
		port = DefaultPort
	}
	body, err := json.Marshal(env)
	if err != nil {
		return mailbox.Receipt{}, err
	}
	url := "http://" + net.JoinHostPort(addr.String(), strconv.Itoa(port)) + "/v1/push"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return mailbox.Receipt{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
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

// Bell is one node's doorbell: a webhook and the key its owner gave us.
type Bell struct {
	URL string
	Key string
}

// Doorbells rings the doorbells this node holds keys for. It implements
// mailbox.Doorbell. For Grok, the bell is Bridge's routine webhook (design
// notes section 9): the JSON body reaches the routine, which hands each
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
	body, _ := json.Marshal(ringBody{Baf: envelope.Version, From: d.From,
		Messages: []ringItem{{ID: env.ID, From: d.From + "/" + env.FromAgent, To: env.To, Kind: string(env.Kind)}}})
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
