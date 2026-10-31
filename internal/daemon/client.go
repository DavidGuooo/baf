package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/DavidGuooo/baf/internal/door"
	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/DavidGuooo/baf/internal/mailbox"
)

func (c *Client) DoorList() ([]door.Connector, error) {
	var list []door.Connector
	return list, c.do("GET", "/v1/door", nil, &list)
}

func (c *Client) DoorRevoke(name string) error {
	a, err := envelope.ParseAddress(name)
	if err != nil {
		return fmt.Errorf("Door agent must be vendor/name: %w", err)
	}
	return c.do("DELETE", "/v1/door/"+url.PathEscape(a.Node)+"/"+url.PathEscape(a.Agent), nil, nil)
}

// Client talks to a running daemon over its Unix socket.
type Client struct {
	http *http.Client
}

// NewClient returns a client for the daemon listening on socket.
func NewClient(socket string) *Client {
	return &Client{http: &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
	}}
}

// ErrNotRunning means nothing is listening on the socket.
var ErrNotRunning = errors.New("the baf daemon is not running (start it with: baf serve)")

// RejectedError is a message the mailbox refused, with its reason.
type RejectedError struct{ Reason string }

func (e *RejectedError) Error() string { return "rejected: " + e.Reason }

func (c *Client) Status() (Status, error) {
	var s Status
	return s, c.do("GET", "/v1/status", nil, &s)
}

func (c *Client) Send(env envelope.Envelope) (mailbox.Receipt, error) {
	var r mailbox.Receipt
	return r, c.do("POST", "/v1/send", env, &r)
}

func (c *Client) AddFile(name string, r io.Reader) (envelope.File, error) {
	var f envelope.File
	err := c.doBody("POST", "/v1/files?"+url.Values{"name": {name}}.Encode(), r, &f)
	return f, err
}

func (c *Client) Inbox(agentName string, all bool) ([]mailbox.Item, error) {
	q := url.Values{"agent": {agentName}}
	if all {
		q.Set("all", "1")
	}
	var items []mailbox.Item
	return items, c.do("GET", "/v1/inbox?"+q.Encode(), nil, &items)
}

func (c *Client) Read(agentName, id string) (mailbox.Item, error) {
	var it mailbox.Item
	path := "/v1/messages/" + url.PathEscape(id) + "?" + url.Values{"agent": {agentName}}.Encode()
	return it, c.do("GET", path, nil, &it)
}

func (c *Client) SentStatus(id string) (mailbox.SentStatus, error) {
	var s mailbox.SentStatus
	return s, c.do("GET", "/v1/sent/"+url.PathEscape(id), nil, &s)
}

func (c *Client) Outbox(all bool) ([]mailbox.OutItem, error) {
	path := "/v1/outbox"
	if all {
		path += "?all=1"
	}
	var items []mailbox.OutItem
	return items, c.do("GET", path, nil, &items)
}

func (c *Client) Runs(limit int) ([]mailbox.RunInfo, error) {
	var runs []mailbox.RunInfo
	return runs, c.do("GET", "/v1/runs?limit="+strconv.Itoa(limit), nil, &runs)
}

func (c *Client) do(method, path string, in, out any) error {
	var body bytes.Buffer
	if in != nil {
		if err := json.NewEncoder(&body).Encode(in); err != nil {
			return err
		}
	}
	return c.doBody(method, path, &body, out)
}

func (c *Client) doBody(method, path string, body io.Reader, out any) error {
	req, err := http.NewRequest(method, "http://baf"+path, body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Op == "dial" {
			return ErrNotRunning
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var e apiError
		json.NewDecoder(resp.Body).Decode(&e)
		if e.Rejected {
			return &RejectedError{Reason: e.Error}
		}
		if e.Error == "" {
			e.Error = resp.Status
		}
		return fmt.Errorf("%s", e.Error)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
