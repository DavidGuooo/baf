package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/DavidGuooo/baf/internal/agent"
	"github.com/DavidGuooo/baf/internal/daemon"
	"github.com/DavidGuooo/baf/internal/door"
	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/DavidGuooo/baf/internal/mailbox"
	"github.com/DavidGuooo/baf/internal/tailnet"
)

type commandDoorIdentity struct{}

func (commandDoorIdentity) SelfPeer(context.Context) (tailnet.Peer, error) {
	return tailnet.Peer{Grants: []tailnet.Grant{{Relay: true, Door: map[string][]string{"openai/dot": {"codex", "me"}}}}}, nil
}

func TestSendCommandToDoorAgent(t *testing.T) {
	// A short directory keeps the socket within macOS's Unix path limit.
	home, err := os.MkdirTemp("/tmp", "baf-door-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	t.Setenv("BAF_HOME", home)
	t.Setenv("BAF_AGENT", "codex")
	mb, err := mailbox.Open(mailbox.Options{
		Path: filepath.Join(home, "mailbox.db"), Node: "box",
		Agents: map[string]agent.Adapter{"codex": nil, "me": nil}, Door: commandDoorIdentity{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mb.Close()
	ln, err := net.Listen("unix", daemon.SocketPath(home))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(daemon.Handler(mb, []string{"codex", "me"}))
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	defer srv.Close()
	if err := send([]string{"openai/dot", "hello from Codex"}, envelope.Notice); err != nil {
		t.Fatal(err)
	}
	items, err := mb.DoorInbox("openai/dot", false)
	if err != nil || len(items) != 1 || items[0].From != "box/codex" || items[0].Message.Body.Text != "hello from Codex" {
		t.Fatalf("inbox = %+v, err = %v", items, err)
	}
	c := daemon.NewClient(daemon.SocketPath(home))
	s, err := c.SentStatus(items[0].Message.ID)
	if err != nil || s.Handover != nil || s.Receiver == nil || s.Receiver.To != "openai/dot" {
		t.Fatalf("status = %+v, err = %v", s, err)
	}
}

func (commandDoorIdentity) WhoIs(context.Context, string) (tailnet.Peer, error) {
	return tailnet.Peer{Node: "phone", Login: "owner@example.com"}, nil
}

func TestDoorCommandsOverSocket(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "baf-door-cmd-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	t.Setenv("BAF_HOME", home)
	mb, err := mailbox.Open(mailbox.Options{Path: filepath.Join(home, "mailbox.db"), Node: "box", Agents: map[string]agent.Adapter{"me": nil}, Door: commandDoorIdentity{}})
	if err != nil {
		t.Fatal(err)
	}
	defer mb.Close()
	d, err := door.Open(door.Options{Path: filepath.Join(home, "door.db"), PublicURL: "https://relay.example.ts.net", SigninURL: "https://relay.example.ts.net:8444", Policy: mb, Identity: commandDoorIdentity{}})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ln, err := net.Listen("unix", daemon.SocketPath(home))
	if err != nil {
		t.Fatal(err)
	}
	local := httptest.NewUnstartedServer(daemon.HandlerWithDoor(mb, []string{"me"}, d))
	local.Listener.Close()
	local.Listener = ln
	local.Start()
	defer local.Close()
	c := daemon.NewClient(daemon.SocketPath(home))
	list, err := c.DoorList()
	if err != nil || len(list) != 1 || list[0].Agent != "openai/dot" || list[0].Connected {
		t.Fatalf("list: %+v %v", list, err)
	}
	// Approve through real HTTP to give the socket command actual tokens to revoke.
	public := httptest.NewServer(d.PublicHandler())
	defer public.Close()
	signin := httptest.NewTLSServer(d.SigninHandler())
	defer signin.Close()
	resp, err := http.Post(public.URL+"/register", "application/json", strings.NewReader(`{"redirect_uris":["https://chatgpt.com/callback"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var registration struct {
		ID string `json:"client_id"`
	}
	err = json.NewDecoder(resp.Body).Decode(&registration)
	resp.Body.Close()
	if err != nil || registration.ID == "" {
		t.Fatalf("register: %+v %v", registration, err)
	}
	verifier := strings.Repeat("a", 43)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {registration.ID}, "redirect_uri": {"https://chatgpt.com/callback"}, "response_type": {"code"}, "resource": {"https://relay.example.ts.net/door/openai/dot/mcp"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}}
	browser := signin.Client()
	browser.Jar, _ = cookiejar.New(nil)
	browser.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err = browser.Get(signin.URL + "/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	match := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindSubmatch(body)
	if len(match) != 2 {
		t.Fatalf("Allow page: %s", body)
	}
	resp, err = browser.PostForm(signin.URL+"/authorize", url.Values{"csrf": {string(match[1])}, "decision": {"allow"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	location, _ := url.Parse(resp.Header.Get("Location"))
	if location == nil || location.Query().Get("code") == "" {
		t.Fatal("no code")
	}
	resp, err = http.PostForm(public.URL+"/token", url.Values{"grant_type": {"authorization_code"}, "code": {location.Query().Get("code")}, "client_id": {registration.ID}, "redirect_uri": {"https://chatgpt.com/callback"}, "code_verifier": {verifier}})
	if err != nil {
		t.Fatal(err)
	}
	var tok struct {
		Access string `json:"access_token"`
	}
	err = json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	if err != nil || tok.Access == "" {
		t.Fatalf("token: %+v %v", tok, err)
	}
	list, err = c.DoorList()
	if err != nil || len(list) != 1 || !list[0].Connected || list[0].ApprovedBy != "owner@example.com" || list[0].ClientID != registration.ID || list[0].State != "connected" {
		t.Fatalf("connected list: %+v %v", list, err)
	}
	for _, args := range [][]string{{"list"}, {"list", "--json"}, {"revoke", "openai/dot"}} {
		if err := doorCommand(args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if _, err := d.Authenticate(tok.Access, "https://relay.example.ts.net/door/openai/dot/mcp"); err == nil {
		t.Fatal("socket revoke left access valid")
	}
	list, err = c.DoorList()
	if err != nil || len(list) != 1 || list[0].Connected || list[0].ApprovedBy != "" {
		t.Fatalf("revoked list: %+v %v", list, err)
	}
	if err := c.DoorRevoke("openai/dot/extra"); err == nil {
		t.Fatal("accepted invalid name")
	}
}
