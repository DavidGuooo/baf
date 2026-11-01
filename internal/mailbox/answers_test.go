package mailbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DavidGuooo/baf/internal/agent"
	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/DavidGuooo/baf/internal/tailnet"
)

func TestAnswerDeadlineOnceAndLateResult(t *testing.T) {
	for _, tc := range []struct{ name, from, to string }{
		{"local", "box/me", "box/other"},
		{"remote", "box/me", "other-node/other"},
		{"to-door", "box/me", "openai/dot"},
		{"door-local", "openai/dot", "box/other"},
		{"door-remote", "openai/dot", "other-node/other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
			opts := answerOptions(t, &now)
			mb := openAnswerMailbox(t, opts)
			defer func() { mb.Close() }()
			from, _ := envelope.ParseAddress(tc.from)
			req := envelope.Envelope{ID: "question", Kind: envelope.Request, FromAgent: from.Agent,
				To: tc.to, FromThread: "sender-thread", Body: envelope.Body{Text: "Please answer this."}}
			var err error
			if from.Node == "box" {
				_, err = mb.Send(req)
			} else {
				_, err = mb.FromDoor(tc.from, req)
			}
			if err != nil {
				t.Fatal(err)
			}
			status := func() SentStatus {
				t.Helper()
				var s SentStatus
				var err error
				if from.Node == "box" {
					s, err = mb.SentStatus(context.Background(), req.ID)
				} else {
					s, err = mb.DoorStatus(context.Background(), tc.from, req.ID)
				}
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
			inbox := from.Agent
			if from.Node != "box" {
				inbox = tc.from
			}
			for _, advance := range []time.Duration{AnswerTimeout - time.Millisecond, time.Millisecond} {
				now = now.Add(advance)
				if err := mb.expireAnswers(); err != nil {
					t.Fatal(err)
				}
				if s := status(); s.AnswerExpiredAt != nil {
					t.Fatalf("expired before more than an hour: %+v", s)
				}
				checkAnswerInbox(t, mb, inbox, 0)
			}
			now = now.Add(time.Millisecond)
			w := &doorWake{}
			mb.SetDoorEvents(w)
			before, err := mb.DoorSequence()
			if err != nil {
				t.Fatal(err)
			}
			if err := mb.expireAnswers(); err != nil {
				t.Fatal(err)
			}
			expired := now
			s := status()
			if s.AnswerExpiredAt == nil || !s.AnswerExpiredAt.Equal(expired) || s.Result != nil {
				t.Fatalf("expired status = %+v", s)
			}
			raw, err := json.Marshal(s)
			if err != nil || !strings.Contains(string(raw), `"answer_expired_at":`) {
				t.Fatalf("status JSON = %s, %v", raw, err)
			}
			items := checkAnswerInbox(t, mb, inbox, 1)
			n := items[0]
			want := fmt.Sprintf("No answer from %s within 1 hour to your request question. It may still answer; status question shows where it is.\n--- your original message ---\nPlease answer this.", tc.to)
			if n.From != "box/baf" || n.Message.Kind != envelope.Notice || n.Message.To != tc.from ||
				n.Message.ToThread != req.FromThread || n.Message.Body.Text != want {
				t.Fatalf("notice = %+v", n)
			}
			if from.Node != "box" {
				events, err := mb.DoorMessages(before)
				if err != nil || len(events) != 1 || events[0].Message.ID != n.Message.ID || w.count == 0 {
					t.Fatalf("Door event = %+v, wakes = %d, err = %v", events, w.count, err)
				}
			}
			now = now.Add(AnswerTimeout)
			if err := mb.expireAnswers(); err != nil {
				t.Fatal(err)
			}
			mb.Close()
			mb = openAnswerMailbox(t, opts)
			checkAnswerInbox(t, mb, inbox, 1)
			if s := status(); s.AnswerExpiredAt == nil || !s.AnswerExpiredAt.Equal(expired) {
				t.Fatalf("lost expiration on reopen: %+v", s)
			}

			to, _ := envelope.ParseAddress(tc.to)
			res := envelope.Envelope{V: envelope.Version, ID: "answer", Kind: envelope.Result,
				FromAgent: to.Agent, To: tc.from, ToThread: req.FromThread, ReplyTo: req.ID,
				Body: envelope.Body{State: envelope.Completed, Text: "Here is the answer."}}
			if _, err := mb.Accept(res, to); err != nil {
				t.Fatalf("late answer refused: %v", err)
			}
			if err := mb.expireAnswers(); err != nil {
				t.Fatal(err)
			}
			checkAnswerInbox(t, mb, inbox, 2)
			s = status()
			if s.Result == nil || s.Result.ID != res.ID || !s.Result.ReceivedAt.Equal(now) ||
				s.AnswerExpiredAt == nil || !s.AnswerExpiredAt.Equal(expired) {
				t.Fatalf("late answer status = %+v", s)
			}
		})
	}
}

func TestAnsweredRequestDoesNotExpire(t *testing.T) {
	for _, door := range []bool{false, true} {
		t.Run(fmt.Sprint(door), func(t *testing.T) {
			now := time.Now().Truncate(time.Millisecond)
			mb := openAnswerMailbox(t, answerOptions(t, &now))
			defer mb.Close()
			req := envelope.Envelope{ID: "question", Kind: envelope.Request, FromAgent: "me", To: "box/other"}
			from, inbox := "box/me", "me"
			var err error
			if door {
				from, inbox = "openai/dot", "openai/dot"
				_, err = mb.FromDoor(from, req)
			} else {
				_, err = mb.Send(req)
			}
			if err != nil {
				t.Fatal(err)
			}
			now = now.Add(30 * time.Minute)
			if _, err := mb.Send(envelope.Envelope{Kind: envelope.Result, FromAgent: "other", To: from,
				ReplyTo: req.ID, Body: envelope.Body{State: envelope.Completed}}); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Hour)
			if err := mb.expireAnswers(); err != nil {
				t.Fatal(err)
			}
			items := checkAnswerInbox(t, mb, inbox, 1)
			if items[0].Message.Kind != envelope.Result {
				t.Fatalf("got notice for answered request: %+v", items)
			}
			// The sweep settles an answered request so later sweeps skip it.
			var closed sql.NullInt64
			if err := mb.db.QueryRow(`SELECT answer_expired_at FROM messages WHERE id = ?`, req.ID).Scan(&closed); err != nil ||
				!closed.Valid || closed.Int64 != noDeadline {
				t.Fatalf("answer_expired_at = %+v, %v", closed, err)
			}
		})
	}
}

func TestRequestsFromBeforeTheDeadlineGetNoNotice(t *testing.T) {
	now := time.Now()
	opts := answerOptions(t, &now)
	mb := openAnswerMailbox(t, opts)
	req, err := mb.Send(envelope.Envelope{Kind: envelope.Request, FromAgent: "me", To: "box/other"})
	if err != nil {
		t.Fatal(err)
	}
	mb.Close()
	db, err := sql.Open("sqlite", opts.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE messages DROP COLUMN answer_expired_at`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	now = now.Add(2 * time.Hour)
	mb = openAnswerMailbox(t, opts)
	defer mb.Close()
	checkAnswerInbox(t, mb, "me", 0)
	s, err := mb.SentStatus(context.Background(), req.ID)
	if err != nil || s.AnswerExpiredAt != nil {
		t.Fatalf("status = %+v, %v", s, err)
	}
}

func TestFailedHandoverDoesNotExpire(t *testing.T) {
	for _, door := range []bool{false, true} {
		t.Run(fmt.Sprint(door), func(t *testing.T) {
			now := time.Now()
			mb := openAnswerMailbox(t, answerOptions(t, &now))
			defer mb.Close()
			req := envelope.Envelope{ID: "question", Kind: envelope.Request, FromAgent: "me", To: "other-node/other"}
			inbox := "me"
			var err error
			if door {
				inbox = "openai/dot"
				_, err = mb.FromDoor(inbox, req)
			} else {
				_, err = mb.Send(req)
			}
			if err != nil {
				t.Fatal(err)
			}
			heads, err := mb.outboxHeads()
			if err != nil || len(heads) != 1 {
				t.Fatalf("heads = %+v, %v", heads, err)
			}
			mb.giveUp(heads[0])
			now = now.Add(2 * time.Hour)
			if err := mb.expireAnswers(); err != nil {
				t.Fatal(err)
			}
			checkAnswerInbox(t, mb, inbox, 1)
			var expired sql.NullInt64
			if err := mb.db.QueryRow(`SELECT answer_expired_at FROM outbox WHERE id = ?`, req.ID).Scan(&expired); err != nil || expired.Valid {
				t.Fatalf("failed request expired = %+v, %v", expired, err)
			}
		})
	}
}

func TestAnswerDeadlineAtStartup(t *testing.T) {
	now := time.Now()
	opts := answerOptions(t, &now)
	mb := openAnswerMailbox(t, opts)
	if _, err := mb.Send(envelope.Envelope{Kind: envelope.Request, FromAgent: "me", To: "box/other"}); err != nil {
		t.Fatal(err)
	}
	mb.Close()
	now = now.Add(2 * time.Hour)
	mb = openAnswerMailbox(t, opts)
	defer mb.Close()
	checkAnswerInbox(t, mb, "me", 1)
}

func TestAnswerDeadlineOnlyForThisSenderMailbox(t *testing.T) {
	now := time.Now()
	mb := openAnswerMailbox(t, answerOptions(t, &now))
	defer mb.Close()
	for _, from := range []Principal{{Node: "other-node", Agent: "me"}, {Node: "box", Agent: SystemAgent}} {
		if _, err := mb.Accept(envelope.Envelope{V: envelope.Version, ID: envelope.NewID(), Kind: envelope.Request,
			To: "box/other"}, from); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := mb.AcceptForwarded(envelope.Envelope{V: envelope.Version, ID: "forwarded", Kind: envelope.Request,
		To: "box/other", Origin: "openai/dot"}, Principal{Node: "openai", Agent: "dot"}, "another-relay"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	if err := mb.expireAnswers(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := mb.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE answer_expired_at IS NOT NULL`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("expired other senders' requests: %d, %v", n, err)
	}
	checkAnswerInbox(t, mb, "openai/dot", 0)
}

func TestAnswerDeadlineWithoutTailscale(t *testing.T) {
	now := time.Now()
	opts := answerOptions(t, &now)
	mb := openAnswerMailbox(t, opts)
	defer mb.Close()
	if _, err := mb.Send(envelope.Envelope{Kind: envelope.Request, FromAgent: "me", To: "box/other"}); err != nil {
		t.Fatal(err)
	}
	opts.Door.(*fakeDoorIdentity).err = errors.New("Tailscale unavailable")
	now = now.Add(2 * time.Hour)
	if err := mb.expireAnswers(); err == nil {
		t.Fatal("lost policy lookup error")
	}
	checkAnswerInbox(t, mb, "me", 1)
}

func answerOptions(t *testing.T, now *time.Time) Options {
	t.Helper()
	return Options{
		Path: filepath.Join(t.TempDir(), "mailbox.db"), Node: "box", Now: func() time.Time { return *now },
		Agents: map[string]agent.Adapter{"me": nil, "other": nil}, Transport: &doorTransport{},
		Door: &fakeDoorIdentity{peer: tailnet.Peer{Node: "box", Grants: []tailnet.Grant{{
			Relay: true, Door: map[string][]string{"openai/dot": {"other", "me"}},
		}}}},
	}
}

func openAnswerMailbox(t *testing.T, opts Options) *Mailbox {
	t.Helper()
	mb, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	return mb
}

func checkAnswerInbox(t *testing.T, mb *Mailbox, agentName string, want int) []Item {
	t.Helper()
	items, err := mb.Inbox(agentName, false, 0)
	if err != nil || len(items) != want {
		t.Fatalf("%s inbox = %+v, err = %v; want %d messages", agentName, items, err, want)
	}
	return items
}
