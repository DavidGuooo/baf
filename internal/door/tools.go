package door

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/DavidGuooo/baf/internal/mailbox"
)

// The local command's JSON endpoint is limited to 1 MiB. Leave a little
// room for MCP's wrapper, while also bounding decoded text separately.
const (
	maxText         = 1 << 20
	maxMCPRequest   = maxText + maxRequest
	writesPerMinute = 30
)

func stringProperty(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func messageProperties() map[string]any {
	return map[string]any{
		"to":          stringProperty("Recipient: node/agent, or just agent for one on this Relay."),
		"text":        stringProperty("Message text, at most 1 MiB in UTF-8; no files."),
		"from_thread": stringProperty("Your own thread id, if you want answers routed back to that thread."),
		"to_thread":   stringProperty("The recipient's thread id, only if that agent has shared it with you."),
	}
}

func replyProperties() map[string]any {
	props := messageProperties()
	delete(props, "to")
	props["reply_to"] = stringProperty("ID of a Request in your inbox. The Result goes to its Principal.")
	props["state"] = map[string]any{
		"type": "string", "enum": []string{"completed", "failed", "rejected"}, "default": "completed",
		"description": "completed: finished; failed: could not finish; rejected: refused before starting.",
	}
	props["to_thread"] = stringProperty("Recipient's shared thread id; defaults to the request's from_thread.")
	return props
}

func tool(name, description string, properties map[string]any, required []string, readOnly bool) map[string]any {
	return map[string]any{
		"name": name, "description": description,
		"inputSchema": map[string]any{
			"type": "object", "properties": properties, "required": required, "additionalProperties": false,
		},
		"annotations": map[string]bool{
			"readOnlyHint": readOnly, "destructiveHint": false, "openWorldHint": false,
		},
	}
}

var mcpTools = []map[string]any{
	tool("send", "Send a Notice to an agent through baf. Use this for information that needs no answer. "+
		"Use request when you need an answer. Returns a storage receipt, not proof the agent has read it.",
		messageProperties(), []string{"to", "text"}, false),
	tool("request", "Ask another agent to do work or answer a question. Returns a receipt immediately; "+
		"the answer arrives later as a Result in inbox. Check status for progress. "+
		"Watch message.created events, then call read with the message id for the full answer; check inbox for missed mail.",
		messageProperties(), []string{"to", "text"}, false),
	tool("reply", "Answer a Request you received with a Result: completed, failed or rejected. "+
		"Name the request in reply_to. Its stored Principal is the recipient. "+
		"Use send for a Notice; never answer a Notice or a Result with reply.",
		replyProperties(), []string{"reply_to", "text"}, false),
	tool("inbox", "List your unread messages, oldest first, including answers to your requests. "+
		"Set all to include messages already read. Listing does not mark mail read; use read for that.",
		map[string]any{"all": map[string]any{"type": "boolean", "default": false}}, []string{}, true),
	tool("outbox", "List messages you recently sent, newest first, to find an id for status. "+
		"Set waiting to list only those not yet handed over.",
		map[string]any{"waiting": map[string]any{"type": "boolean", "default": false}}, []string{}, true),
	tool("read", "Open one message addressed to you and mark it read. This neither deletes it nor answers it. "+
		"If it is a Request, use reply when you have an answer. Files cannot be downloaded through this door.",
		map[string]any{"id": stringProperty("ID of a message in your inbox.")}, []string{"id"}, false),
	tool("status", "Check a message you sent: whether it was opened, any run progress, and the Result id "+
		"once a request is answered. Use inbox or read to get the Result's full text.",
		map[string]any{"id": stringProperty("ID from outbox or a receipt for a message you sent.")}, []string{"id"}, true),
}

type toolArguments struct {
	To         string               `json:"to"`
	Text       string               `json:"text"`
	FromThread string               `json:"from_thread"`
	ToThread   string               `json:"to_thread"`
	ReplyTo    string               `json:"reply_to"`
	State      envelope.ResultState `json:"state"`
	ID         string               `json:"id"`
	All        bool                 `json:"all"`
	Waiting    bool                 `json:"waiting"`
}

func (s *Server) callTool(ctx context.Context, principal, name string, raw map[string]json.RawMessage) any {
	// Decode only this tool's declared arguments. In particular, no sender,
	// origin or envelope supplied by the client can replace the token's Principal.
	var properties map[string]any
	var required []string
	for _, definition := range mcpTools {
		if definition["name"] == name {
			schema := definition["inputSchema"].(map[string]any)
			properties = schema["properties"].(map[string]any)
			required = schema["required"].([]string)
			break
		}
	}
	for _, key := range required {
		if _, ok := raw[key]; !ok {
			return toolResult(nil, fmt.Errorf("%s is required", key))
		}
	}
	args := map[string]json.RawMessage{}
	for key := range properties {
		if value, ok := raw[key]; ok {
			if string(value) == "null" {
				return toolResult(nil, fmt.Errorf("%s must not be null", key))
			}
			args[key] = value
		}
	}
	data, _ := json.Marshal(args)
	var a toolArguments
	if err := json.Unmarshal(data, &a); err != nil {
		return toolResult(nil, fmt.Errorf("bad tool arguments: %w", err))
	}
	if s.mailbox == nil {
		return toolResult(nil, errors.New("the Public door has no mailbox"))
	}
	if name != "inbox" && name != "outbox" && name != "status" && !s.allowWrite(principal) {
		return toolResult(nil, errors.New("too many writes: a Door agent may write 30 times a minute; "+
			"try again shortly"))
	}
	if _, ok := raw["files"]; ok {
		return toolResult(nil, errors.New("files from Door agents are not supported yet"))
	}
	switch name {
	case "inbox":
		items, err := s.mailbox.DoorInbox(principal, a.All)
		if items == nil {
			items = []mailbox.Item{}
		}
		for i := range items {
			items[i].Files = nil // Local paths are useful only to agents on the Relay.
		}
		return toolResult(map[string]any{"messages": items}, err)
	case "outbox":
		items, err := s.mailbox.OutboxAs(principal, !a.Waiting, 50)
		if items == nil {
			items = []mailbox.OutItem{}
		}
		return toolResult(map[string]any{"messages": items}, err)
	case "read":
		item, err := s.mailbox.DoorRead(principal, a.ID)
		item.Files = nil
		if err == nil {
			item.Read = true
		}
		return toolResult(item, err)
	case "status":
		status, err := s.mailbox.DoorStatus(ctx, principal, a.ID)
		return toolResult(status, err)
	}
	if len(a.Text) > maxText {
		return toolResult(nil, errors.New("message text exceeds the 1 MiB limit"))
	}
	env := envelope.Envelope{
		Kind: envelope.Notice, To: a.To, FromThread: a.FromThread, ToThread: a.ToThread,
		Body: envelope.Body{Text: a.Text},
	}
	if name == "request" {
		env.Kind = envelope.Request
	}
	if name == "reply" {
		if a.State == "" {
			a.State = envelope.Completed
		}
		if a.State != envelope.Completed && a.State != envelope.Failed && a.State != envelope.Rejected {
			return toolResult(nil, fmt.Errorf("unknown result state %q", a.State))
		}
		request, err := s.receivedRequest(principal, a.ReplyTo)
		if err != nil {
			return toolResult(nil, err)
		}
		env.Kind, env.ReplyTo, env.Body.State = envelope.Result, a.ReplyTo, a.State
		env.To = request.From
		if _, supplied := raw["to_thread"]; !supplied {
			env.ToThread = request.Message.FromThread
		}
	}
	if !strings.Contains(env.To, "/") {
		env.To = s.mailbox.Node() + "/" + env.To
	}
	receipt, err := s.mailbox.FromDoor(principal, env)
	return toolResult(receipt, err)
}

func (s *Server) receivedRequest(principal, id string) (mailbox.Item, error) {
	// Listing keeps reply from marking a request read as a side effect. It also
	// prevents replying to a request addressed to any other agent.
	items, err := s.mailbox.DoorInbox(principal, true)
	if err != nil {
		return mailbox.Item{}, err
	}
	for _, item := range items {
		if item.Message.ID == id {
			if item.Message.Kind != envelope.Request {
				return mailbox.Item{}, errors.New("reply_to must name a request you received")
			}
			return item, nil
		}
	}
	return mailbox.Item{}, mailbox.ErrNotFound
}

func (s *Server) allowWrite(principal string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.now().Add(-time.Minute)
	// Forget inactive names too, so revoked connectors do not accumulate here.
	for name, times := range s.writes {
		for len(times) > 0 && !times[0].After(cutoff) {
			times = times[1:]
		}
		if len(times) == 0 {
			delete(s.writes, name)
		} else {
			s.writes[name] = times
		}
	}
	if len(s.writes[principal]) >= writesPerMinute {
		return false
	}
	s.writes[principal] = append(s.writes[principal], s.now())
	return true
}
