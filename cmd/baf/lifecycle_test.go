package main

import (
	"slices"
	"testing"

	"github.com/DavidGuooo/baf/internal/mailbox"
)

func TestWaitingFrom(t *testing.T) {
	items := []mailbox.OutItem{
		{ID: "01A", FromAgent: "tokdev", State: "waiting"},
		{ID: "01B", FromAgent: "bridge", State: "waiting"},
		{ID: "01C", FromAgent: "tokdev", State: "handed"},
		{ID: "01D", FromAgent: "tokdev", State: "failed"},
		{ID: "01E", FromAgent: "tokdev", State: "waiting"},
	}
	if got := waitingFrom(items, "tokdev"); !slices.Equal(got, []string{"01A", "01E"}) {
		t.Errorf("tokdev: got %v, want only its waiting messages", got)
	}
	if got := waitingFrom(items, "codex"); got != nil {
		t.Errorf("codex: got %v, want nothing", got)
	}
}
