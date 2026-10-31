package door

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/DavidGuooo/baf/internal/mailbox"
)

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type callResult struct {
	ResultType string `json:"resultType"`
	IsError    bool   `json:"isError"`
	Content    []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Structured json.RawMessage `json:"structuredContent"`
}

func (h *harness) rpc(name, token, body string, status int) rpcResponse {
	h.t.Helper()
	resp, data := h.request(h.public.Client(), "POST", h.public.URL+"/door/"+name+"/mcp", body,
		map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json"})
	if resp.StatusCode != status {
		h.t.Fatalf("RPC status = %d, want %d: %s", resp.StatusCode, status, data)
	}
	var out rpcResponse
	if err := json.Unmarshal([]byte(data), &out); err != nil {
		h.t.Fatal(err)
	}
	if out.JSONRPC != "2.0" {
		h.t.Fatalf("not JSON-RPC: %s", data)
	}
	return out
}

func (h *harness) call(name, token, tool string, args any) callResult {
	h.t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": "call", "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	out := h.rpc(name, token, string(body), 200)
	if out.Error != nil || string(out.ID) != `"call"` {
		h.t.Fatalf("tool returned RPC error or wrong id: %+v", out)
	}
	var result callResult
	if err := json.Unmarshal(out.Result, &result); err != nil {
		h.t.Fatal(err)
	}
	if result.ResultType != "complete" || len(result.Content) != 1 || result.Content[0].Type != "text" {
		h.t.Fatalf("bad tool result: %s", out.Result)
	}
	return result
}

func decodeResult[T any](t *testing.T, result callResult) T {
	t.Helper()
	if result.IsError {
		t.Fatalf("tool failed: %+v", result.Content)
	}
	var value T
	if err := json.Unmarshal(result.Structured, &value); err != nil {
		t.Fatal(err)
	}
	if result.Content[0].Text != string(result.Structured) {
		t.Fatal("text and structured content differ")
	}
	return value
}

func wantToolError(t *testing.T, result callResult, reason string) {
	t.Helper()
	if !result.IsError || !strings.Contains(result.Content[0].Text, reason) {
		t.Fatalf("want tool error %q, got %+v", reason, result)
	}
}

func TestMCPDiscovery(t *testing.T) {
	h := newHarness(t)
	tok := h.token(codeForm(h.code("openai/dot")), 200)
	out := h.rpc("openai/dot", tok.Access, `{"jsonrpc":"2.0","id":1,"method":"server/discover"}`, 200)
	var discovery struct {
		ResultType        string                     `json:"resultType"`
		SupportedVersions []string                   `json:"supportedVersions"`
		Capabilities      map[string]json.RawMessage `json:"capabilities"`
		Meta              map[string]json.RawMessage `json:"_meta"`
		Instructions      string                     `json:"instructions"`
		TTL               int                        `json:"ttlMs"`
		CacheScope        string                     `json:"cacheScope"`
	}
	if err := json.Unmarshal(out.Result, &discovery); err != nil {
		t.Fatal(err)
	}
	if out.Error != nil || discovery.ResultType != "complete" || len(discovery.SupportedVersions) != 4 ||
		discovery.SupportedVersions[0] != modernVersion || discovery.TTL != 60000 ||
		discovery.CacheScope != "private" || discovery.Instructions == "" ||
		discovery.Meta["io.modelcontextprotocol/serverInfo"] == nil ||
		discovery.Capabilities["tools"] == nil || discovery.Capabilities["events"] == nil {
		t.Fatalf("discovery = %s", out.Result)
	}
	out = h.rpc("openai/dot", tok.Access, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, 200)
	var list struct {
		ResultType string `json:"resultType"`
		Tools      []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
			Annotations map[string]bool `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out.Result, &list); err != nil {
		t.Fatal(err)
	}
	if list.ResultType != "complete" || len(list.Tools) != 6 {
		t.Fatalf("tools = %s", out.Result)
	}
	for i, name := range []string{"send", "request", "reply", "inbox", "read", "status"} {
		tool := list.Tools[i]
		if tool.Name != name || tool.Description == "" || len(tool.InputSchema) == 0 ||
			tool.Annotations["readOnlyHint"] != (name == "inbox" || name == "status") {
			t.Fatalf("bad tool: %+v", tool)
		}
		for _, key := range []string{"destructiveHint", "openWorldHint"} {
			if v, exists := tool.Annotations[key]; !exists || v {
				t.Fatalf("%s annotation %s = %v, exists = %v", name, key, v, exists)
			}
		}
	}
	out = h.rpc("openai/dot", tok.Access, `{"jsonrpc":"2.0","id":3,"method":"events/list"}`, 200)
	var events struct {
		Events []any `json:"events"`
	}
	if err := json.Unmarshal(out.Result, &events); err != nil || len(events.Events) != 1 {
		t.Fatalf("events = %s, err = %v", out.Result, err)
	}
	for _, version := range append(append([]string{}, legacyVersions...), "unsupported") {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":"legacy","method":"initialize",`+
			`"params":{"protocolVersion":%q}}`, version)
		out = h.rpc("openai/dot", tok.Access, body, 200)
		if version == "unsupported" {
			version = legacyVersions[0]
		}
		if out.Error != nil || !strings.Contains(string(out.Result), `"protocolVersion":"`+version+`"`) {
			t.Fatalf("legacy = %s, error = %+v", out.Result, out.Error)
		}
	}
}

func TestMCPNoticePrincipalAndPolicy(t *testing.T) {
	h := newHarness(t)
	tok := h.token(codeForm(h.code("openai/dot")), 200)
	result := h.call("openai/dot", tok.Access, "send", map[string]any{
		"to": "codex", "text": "hello", "from_thread": "dot-thread",
		"from": "relay/claude", "from_agent": "claude", "principal": "relay/claude",
		"origin": "other/me", "kind": "request",
	})
	receipt := decodeResult[mailbox.Receipt](t, result)
	items, err := h.mailbox.Inbox("codex", true)
	if err != nil || len(items) != 1 || items[0].From != "openai/dot" ||
		items[0].Message.ID != receipt.ID || items[0].Message.FromAgent != "dot" ||
		items[0].Message.Origin != "" || items[0].Message.Kind != envelope.Notice ||
		items[0].Message.FromThread != "dot-thread" || items[0].Message.Body.Text != "hello" {
		t.Fatalf("stored message = %+v, err = %v", items, err)
	}
	for _, tc := range []struct{ to, reason string }{
		{"claude", `its door entry in the Tailscale policy does not allow "claude"`},
		{"other/codex", "Relay doesn't forward for Door agents yet"},
		{"openai/other", "Relay doesn't forward for Door agents yet"},
	} {
		wantToolError(t, h.call("openai/dot", tok.Access, "send",
			map[string]any{"to": tc.to, "text": "no"}), tc.reason)
	}
	other := h.token(codeForm(h.code("openai/other")), 200)
	wantToolError(t, h.call("openai/other", other.Access, "status",
		map[string]any{"id": receipt.ID}), "no such message")
	status := decodeResult[mailbox.SentStatus](t, h.call("openai/dot", tok.Access, "status",
		map[string]any{"id": receipt.ID}))
	if status.Receiver == nil || status.Receiver.ReadAt != nil {
		t.Fatalf("unread status = %+v", status)
	}
	if _, err := h.mailbox.Read("codex", receipt.ID); err != nil {
		t.Fatal(err)
	}
	status = decodeResult[mailbox.SentStatus](t, h.call("openai/dot", tok.Access, "status",
		map[string]any{"id": receipt.ID}))
	if status.Receiver.ReadAt == nil {
		t.Fatal("status did not report that codex opened the notice")
	}
}

func TestMCPRequestResultInboxRead(t *testing.T) {
	h := newHarness(t)
	tok := h.token(codeForm(h.code("openai/dot")), 200)
	receipt := decodeResult[mailbox.Receipt](t, h.call("openai/dot", tok.Access, "request",
		map[string]any{"to": "relay/codex", "text": "What is 2+2?", "from_thread": "dot-thread"}))
	result, err := h.mailbox.Send(envelope.Envelope{
		Kind: envelope.Result, FromAgent: "codex", To: "openai/dot", ReplyTo: receipt.ID,
		FromThread: "codex-thread", ToThread: "dot-thread",
		Body: envelope.Body{Text: "4", State: envelope.Completed},
	})
	if err != nil {
		t.Fatal(err)
	}
	type inbox struct {
		Messages []mailbox.Item `json:"messages"`
	}
	messages := decodeResult[inbox](t, h.call("openai/dot", tok.Access, "inbox", map[string]any{}))
	if len(messages.Messages) != 1 || messages.Messages[0].Read ||
		messages.Messages[0].Message.Kind != envelope.Result || messages.Messages[0].Message.Body.Text != "4" ||
		messages.Messages[0].Message.ReplyTo != receipt.ID || messages.Messages[0].Message.ToThread != "dot-thread" {
		t.Fatalf("inbox = %+v", messages)
	}
	item := decodeResult[mailbox.Item](t, h.call("openai/dot", tok.Access, "read",
		map[string]any{"id": result.ID, "agent": "codex"}))
	if !item.Read || item.From != "relay/codex" {
		t.Fatalf("read = %+v", item)
	}
	messages = decodeResult[inbox](t, h.call("openai/dot", tok.Access, "inbox", map[string]any{}))
	if len(messages.Messages) != 0 {
		t.Fatalf("read message still unread: %+v", messages)
	}
	messages = decodeResult[inbox](t, h.call("openai/dot", tok.Access, "inbox", map[string]any{"all": true}))
	if len(messages.Messages) != 1 {
		t.Fatal("read deleted the message")
	}
	status, err := h.mailbox.SentStatus(context.Background(), result.ID)
	if err != nil || status.Receiver == nil || status.Receiver.ReadAt == nil {
		t.Fatalf("local sender cannot see Door read: %+v, %v", status, err)
	}
	sent := decodeResult[mailbox.SentStatus](t, h.call("openai/dot", tok.Access, "status",
		map[string]any{"id": receipt.ID}))
	if sent.Result == nil || sent.Result.ID != result.ID {
		t.Fatalf("request status = %+v", sent)
	}
	wantToolError(t, h.call("openai/dot", tok.Access, "read",
		map[string]any{"id": receipt.ID}), "not addressed to you")
}

func TestMCPReply(t *testing.T) {
	for _, state := range []string{"", "completed", "failed", "rejected"} {
		t.Run("state="+state, func(t *testing.T) {
			h := newHarness(t)
			tok := h.token(codeForm(h.code("openai/dot")), 200)
			req, err := h.mailbox.Send(envelope.Envelope{
				Kind: envelope.Request, FromAgent: "codex", To: "openai/dot", FromThread: "codex-thread",
				Body: envelope.Body{Text: "Please answer"},
			})
			if err != nil {
				t.Fatal(err)
			}
			receipt := decodeResult[mailbox.Receipt](t, h.call("openai/dot", tok.Access, "reply", map[string]any{
				"reply_to": req.ID, "text": "answer", "state": state, "from_thread": "dot-thread",
				"to": "other/claude", "from_agent": "claude",
			}))
			items, err := h.mailbox.Inbox("codex", true)
			if err != nil || len(items) != 1 {
				t.Fatalf("inbox = %+v, %v", items, err)
			}
			if state == "" {
				state = "completed"
			}
			got := items[0]
			if got.From != "openai/dot" || got.Message.Kind != envelope.Result ||
				got.Message.ID != receipt.ID || got.Message.ReplyTo != req.ID ||
				string(got.Message.Body.State) != state || got.Message.ToThread != "codex-thread" ||
				got.Message.FromThread != "dot-thread" {
				t.Fatalf("reply = %+v", got)
			}
			unread, err := h.mailbox.DoorInbox("openai/dot", false)
			if err != nil || len(unread) != 1 {
				t.Fatal("reply marked request read", err)
			}
		})
	}
}

func TestMCPToolFailuresAndFiles(t *testing.T) {
	h := newHarness(t)
	tok := h.token(codeForm(h.code("openai/dot")), 200)
	for _, tc := range []struct {
		tool   string
		args   map[string]any
		reason string
	}{
		{"send", map[string]any{"to": "me"}, "text is required"},
		{"send", map[string]any{"to": "me", "text": 7}, "bad tool arguments"},
		{"send", map[string]any{"to": "me", "text": nil}, "text must not be null"},
		{"send", map[string]any{"to": "me", "text": "x", "files": []any{}}, "files from Door agents"},
		{"reply", map[string]any{"reply_to": "missing", "text": "x"}, "no such message"},
		{"reply", map[string]any{"reply_to": "missing", "text": "x", "state": "unknown"}, "unknown result state"},
		{"read", map[string]any{"id": "missing"}, "no such message"},
		{"inbox", map[string]any{"all": "yes"}, "bad tool arguments"},
	} {
		wantToolError(t, h.call("openai/dot", tok.Access, tc.tool, tc.args), tc.reason)
	}
	file, err := h.mailbox.AddFile("hello.txt", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := h.mailbox.Send(envelope.Envelope{
		Kind: envelope.Notice, FromAgent: "codex", To: "openai/dot",
		Body: envelope.Body{Text: "file unavailable through door", Files: []envelope.File{file}},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantToolError(t, h.call("openai/dot", tok.Access, "reply",
		map[string]any{"reply_to": r.ID, "text": "no"}), "reply_to must name a request")
	for _, tool := range []string{"inbox", "read"} {
		result := h.call("openai/dot", tok.Access, tool, map[string]any{"id": r.ID})
		if result.IsError || strings.Contains(string(result.Structured), `"path"`) {
			t.Fatalf("%s exposed local file paths: %s", tool, result.Structured)
		}
	}
}

func TestMCPRateLimit(t *testing.T) {
	h := newHarness(t)
	tok := h.token(codeForm(h.code("openai/dot")), 200)
	var wg sync.WaitGroup
	results := make(chan callResult, writesPerMinute+5)
	for range writesPerMinute + 5 {
		wg.Go(func() {
			results <- h.call("openai/dot", tok.Access, "send", map[string]any{"to": "me", "text": "hello"})
		})
	}
	wg.Wait()
	close(results)
	accepted := 0
	var sentID string
	for result := range results {
		if result.IsError {
			wantToolError(t, result, "30 times a minute")
		} else {
			accepted++
			sentID = decodeResult[mailbox.Receipt](t, result).ID
		}
	}
	if accepted != writesPerMinute {
		t.Fatalf("accepted %d writes", accepted)
	}
	tok = h.token(refreshForm(tok), 200)
	wantToolError(t, h.call("openai/dot", tok.Access, "send",
		map[string]any{"to": "me", "text": "still limited"}), "30 times a minute")
	decodeResult[map[string]any](t, h.call("openai/dot", tok.Access, "inbox", map[string]any{}))
	decodeResult[mailbox.SentStatus](t, h.call("openai/dot", tok.Access, "status",
		map[string]any{"id": sentID}))
	incoming, err := h.mailbox.Send(envelope.Envelope{
		Kind: envelope.Request, FromAgent: "me", To: "openai/dot", Body: envelope.Body{Text: "hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"read", "reply", "request"} {
		wantToolError(t, h.call("openai/dot", tok.Access, tool, map[string]any{
			"id": incoming.ID, "reply_to": incoming.ID, "to": "me", "text": "limited",
		}), "30 times a minute")
	}
	unread, err := h.mailbox.DoorInbox("openai/dot", false)
	if err != nil || len(unread) != 1 {
		t.Fatalf("limited read changed inbox: %+v, %v", unread, err)
	}
	other := h.token(codeForm(h.code("openai/other")), 200)
	decodeResult[mailbox.Receipt](t, h.call("openai/other", other.Access, "send",
		map[string]any{"to": "me", "text": "separate limit"}))
	h.clock.Add(60)
	decodeResult[mailbox.Receipt](t, h.call("openai/dot", tok.Access, "send",
		map[string]any{"to": "me", "text": "allowed again"}))
}

func TestMCPProtocolErrorsAndLimits(t *testing.T) {
	h := newHarness(t)
	tok := h.token(codeForm(h.code("openai/dot")), 200)
	for _, tc := range []struct {
		body         string
		status, code int
	}{
		{``, 400, -32700},
		{`{`, 400, -32700},
		{`{} {}`, 400, -32700},
		{`{"jsonrpc":"2.0","id":1,"method":"ping"} junk`, 400, -32700},
		{`null`, 400, -32600},
		{`[]`, 400, -32600},
		{`{"jsonrpc":"1.0","id":1,"method":"ping"}`, 400, -32600},
		{`{"jsonrpc":"2.0","id":true,"method":"ping"}`, 400, -32600},
		{`{"jsonrpc":"2.0","id":1,"method":7}`, 400, -32600},
		{`{"jsonrpc":"2.0","id":1,"method":"missing"}`, 200, -32601},
		{`{"jsonrpc":"2.0","id":1,"method":"ping","params":[]}`, 200, -32602},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"bad"}}`, 200, -32602},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send","arguments":[]}}`, 200, -32602},
	} {
		out := h.rpc("openai/dot", tok.Access, tc.body, tc.status)
		if out.Error == nil || out.Error.Code != tc.code || out.Result != nil {
			t.Fatalf("%s: %+v", tc.body, out)
		}
		if tc.status == 400 && string(out.ID) != "null" {
			t.Fatalf("invalid request echoed an id: %s", out.ID)
		}
	}
	for _, body := range []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"send","arguments":{"to":"me","text":"x"}}}`,
	} {
		resp, data := h.request(h.public.Client(), "POST", h.public.URL+"/door/openai/dot/mcp", body,
			map[string]string{"Authorization": "Bearer " + tok.Access})
		if resp.StatusCode != 202 || data != "" {
			t.Fatalf("notification: %d %s", resp.StatusCode, data)
		}
	}
	if items, err := h.mailbox.Inbox("me", true); err != nil || len(items) != 0 {
		t.Fatal("notification sent mail", items, err)
	}
	for _, method := range []string{"GET", "DELETE"} {
		resp, _ := h.request(h.public.Client(), method, h.public.URL+"/door/openai/dot/mcp", "",
			map[string]string{"Authorization": "Bearer " + tok.Access})
		if resp.StatusCode != 405 || resp.Header.Get("Allow") != "POST" {
			t.Fatal(resp.Status)
		}
	}
	// Both known-length and chunked oversized bodies are refused.
	for _, chunked := range []bool{false, true} {
		req, err := http.NewRequest("POST", h.public.URL+"/door/openai/dot/mcp",
			strings.NewReader(strings.Repeat("x", maxMCPRequest+1)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+tok.Access)
		if chunked {
			req.ContentLength = -1
		}
		resp, err := h.public.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 413 {
			t.Fatal(resp.Status)
		}
	}
	decodeResult[mailbox.Receipt](t, h.call("openai/dot", tok.Access, "send",
		map[string]any{"to": "me", "text": strings.Repeat("x", maxText)}))
	wantToolError(t, h.call("openai/dot", tok.Access, "send",
		map[string]any{"to": "me", "text": strings.Repeat("x", maxText+1)}), "text exceeds")
	h.bearer("openai/dot", "", 401)
	h.bearer("openai/other", tok.Access, 401)
}
