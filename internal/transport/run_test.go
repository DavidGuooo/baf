package transport

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DavidGuooo/baf/internal/agent"
	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/DavidGuooo/baf/internal/mailbox"
	"github.com/DavidGuooo/baf/internal/tailnet"
)

func TestPushRunAuthorization(t *testing.T) {
	home := t.TempDir()
	mb, err := mailbox.Open(mailbox.Options{Path: filepath.Join(home, "mail.db"), Node: "box", UserHome: home, Agents: map[string]agent.Adapter{"claude": agent.Claude{}, "me": nil}})
	if err != nil {
		t.Fatal(err)
	}
	defer mb.Close() // no dispatcher: no real agent or opener can run
	id := &fakeIdentity{peer: tailnet.Peer{Node: "grokbot", Grants: []tailnet.Grant{{To: []string{"claude"}}}}}
	handler := Handler(mb, id)
	for _, tc := range []struct {
		dirs   []string
		origin string
		run    *envelope.Run
		want   string
	}{
		{nil, "", &envelope.Run{Dir: "repo/new"}, "dirs"},
		{[]string{"repo"}, "", &envelope.Run{Dir: "outside"}, "dirs"},
		{[]string{"repo"}, "", &envelope.Run{Dir: "repo/new"}, ""},
		{nil, "", &envelope.Run{Desktop: true}, ""},
		{[]string{"*"}, "openai/dot", &envelope.Run{Desktop: true}, "origin"},
	} {
		id.peer.Grants[0].Dirs = tc.dirs
		env := envelope.Envelope{V: 1, ID: envelope.NewID(), Kind: envelope.Notice, To: "box/claude", FromAgent: "bot", Origin: tc.origin, Run: tc.run}
		raw, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/push", bytes.NewReader(raw)))
		var response pushResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if tc.want == "" {
			if rr.Code != 200 || response.Receipt == nil {
				t.Fatalf("response=%d %s", rr.Code, rr.Body)
			}
		} else if !response.Rejected || !strings.Contains(response.Error, tc.want) {
			t.Fatalf("response=%d %s", rr.Code, rr.Body)
		}
	}
	items, err := mb.Inbox("claude", false, 10)
	if err != nil || len(items) != 2 {
		t.Fatalf("stored rejected message: count=%d error=%v", len(items), err)
	}
}
