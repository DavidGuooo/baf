package mailbox

import (
	"context"
	"testing"
	"time"

	"github.com/DavidGuooo/baf/internal/envelope"
)

type doorWake struct{ count int }

func (w *doorWake) Wake() { w.count++ }

func TestDoorEventsIncludeAutomaticResults(t *testing.T) {
	mb, _, _ := doorMailbox(t, true)
	w := &doorWake{}
	mb.SetDoorEvents(w)
	r, err := mb.FromDoor("openai/dot", envelope.Envelope{Kind: envelope.Request, To: "box/codex"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := mb.DoorSequence()
	if err != nil {
		t.Fatal(err)
	}
	claims, err := mb.claim()
	if err != nil || len(claims) != 1 {
		t.Fatalf("claims: %+v %v", claims, err)
	}
	w.count = 0
	mb.execute(context.Background(), claims[0])
	if w.count == 0 {
		t.Fatal("automatic Result did not wake the door")
	}
	messages, err := mb.DoorMessages(before)
	if err != nil || len(messages) != 1 {
		t.Fatalf("messages: %+v %v", messages, err)
	}
	m := messages[0]
	if m.Sequence <= before || m.From != "box/codex" || m.Message.Kind != envelope.Result ||
		m.Message.ReplyTo != r.ID || m.Message.Body.State != envelope.Completed {
		t.Fatalf("wrong automatic Result: %+v", m)
	}
	if again, err := mb.DoorMessages(m.Sequence); err != nil || len(again) != 0 {
		t.Fatalf("repeated feed: %+v %v", again, err)
	}
	mb.SetDoorEvents(nil)
}

func TestDoorNotifiedScopesMessageAndSurvivesReopen(t *testing.T) {
	mb, _, _ := doorMailbox(t, true)
	for _, sender := range []string{"one", "two"} {
		_, err := mb.Accept(envelope.Envelope{V: envelope.Version, ID: "same-id", Kind: envelope.Notice,
			FromAgent: "me", To: "openai/dot"}, Principal{Node: sender, Agent: "me"})
		if err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().Truncate(time.Millisecond)
	for _, sender := range []string{"one/wrong", "one/me"} {
		if err := mb.DoorNotified("openai/dot", sender, "same-id", now); err != nil {
			t.Fatal(err)
		}
	}
	if err := mb.DoorNotified("openai/other", "two/me", "same-id", now); err != nil {
		t.Fatal(err)
	}
	if err := mb.DoorNotified("openai/dot", "one/me", "same-id", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Reopen the same database, as on daemon startup.
	var path string
	if err := mb.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := mb.db.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	mb.db, err = openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, sender := range []string{"one", "two"} {
		p, err := mb.Progress(sender, "same-id")
		if err != nil {
			t.Fatal(err)
		}
		if p.ReadAt != nil || p.State != "inbox" {
			t.Fatalf("mail changed: %+v", p)
		}
		if sender == "one" {
			if p.NotifiedAt == nil || !p.NotifiedAt.Equal(now) {
				t.Fatalf("first time lost: %+v", p)
			}
		} else if p.NotifiedAt != nil {
			t.Fatalf("other sender changed: %+v", p)
		}
	}
}
