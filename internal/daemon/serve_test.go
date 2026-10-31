package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/DavidGuooo/baf/internal/config"
)

func TestDoorSettingsCheckedBeforeStartup(t *testing.T) {
	for _, tc := range []struct {
		door config.DoorConfig
		port int
		want string
	}{
		{config.DoorConfig{Enabled: true}, 0, "door.public_url"},
		{config.DoorConfig{Enabled: true, PublicURL: "https://relay.example.ts.net", SigninPort: 7780}, 0, "mailbox port"},
		{config.DoorConfig{Enabled: true, PublicURL: "https://relay.example.ts.net", SigninPort: 9000}, 9000, "mailbox port"},
	} {
		// A nil Tailscale client also proves validation happens before any API use.
		err := Serve(context.Background(), t.TempDir(), config.Config{Door: tc.door, Port: tc.port}, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("error = %v, want %q", err, tc.want)
		}
	}
}
