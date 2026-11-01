package mailbox

import (
	"fmt"
	"testing"

	"github.com/DavidGuooo/baf/internal/envelope"
)

func TestInboxOrderAndLimit(t *testing.T) {
	for _, name := range []string{"me", "openai/dot"} {
		t.Run(name, func(t *testing.T) {
			mb, _, _ := doorMailbox(t, true)
			to := "box/me"
			list := func(unread bool, limit int) ([]Item, error) { return mb.Inbox(name, unread, limit) }
			if name == "openai/dot" {
				to = name
				list = func(unread bool, limit int) ([]Item, error) { return mb.DoorInbox(name, unread, limit) }
			}
			for i := 0; i < 60; i++ {
				if _, err := mb.Send(envelope.Envelope{ID: fmt.Sprintf("mail-%02d", i), Kind: envelope.Notice, FromAgent: "me", To: to}); err != nil {
					t.Fatal(err)
				}
			}
			// Read mail at both ends, and another agent's newer mail, must be handled correctly.
			for _, id := range []string{"mail-00", "mail-59"} {
				if _, err := mb.Read(name, id); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := mb.Send(envelope.Envelope{ID: "other-mail", Kind: envelope.Notice, FromAgent: "me", To: "box/claude"}); err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name                      string
				unread                    bool
				limit, count, first, step int
			}{
				{"recent", false, 50, 50, 59, -1},
				{"small limit", false, 2, 2, 59, -1},
				{"unlimited", false, 0, 60, 59, -1},
				{"unread ignores limit", true, 2, 58, 1, 1},
				{"unread unlimited", true, 0, 58, 1, 1},
			} {
				t.Run(tc.name, func(t *testing.T) {
					items, err := list(tc.unread, tc.limit)
					if err != nil || len(items) != tc.count {
						t.Fatalf("got %d items, err %v; want %d", len(items), err, tc.count)
					}
					for i, item := range items {
						n := tc.first + i*tc.step
						if item.Message.ID != fmt.Sprintf("mail-%02d", n) || item.Read != (n == 0 || n == 59) {
							t.Fatalf("item %d = %+v", i, item)
						}
					}
				})
			}
		})
	}
}
