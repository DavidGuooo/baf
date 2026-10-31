package mailbox

import (
	"context"
	"testing"

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
