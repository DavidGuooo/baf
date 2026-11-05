package daemon

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DavidGuooo/baf/internal/config"
)

func TestScratchFolder(t *testing.T) {
	userHome, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(userHome, "baf")
	account, org := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	settings := `{"oauthAccount":{"accountUuid":"` + account + `","organizationUuid":"` + org + `"}}`
	now := time.Date(2026, 11, 5, 1, 0, 0, 0, time.FixedZone("CST", 8*3600))
	for _, tc := range []struct {
		name, goos, data string
		desktop          bool
	}{
		{"mac", "darwin", settings, true},
		{"linux", "linux", settings, false},
		{"missing", "darwin", "", false},
		{"broken", "darwin", "{", false},
		{"no fields", "darwin", "{}", false},
		{"missing org", "darwin", `{"oauthAccount":{"accountUuid":"` + account + `"}}`, false},
		{"unsafe field", "darwin", `{"oauthAccount":{"accountUuid":"../bad","organizationUuid":"` + org + `"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(userHome, ".claude.json")
			if tc.data == "" {
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			dir, err := scratchDir(home, userHome, tc.goos, "claude", now)
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(home, "work", "claude")
			if tc.desktop {
				root = filepath.Join(userHome, "Library", "Application Support", "Claude", "scratch-workspaces", account, org)
			}
			if filepath.Dir(dir) != root || !regexp.MustCompile(`^scratch-2026-11-04-[0-9a-f]{6}$`).MatchString(filepath.Base(dir)) {
				t.Fatalf("dir=%s", dir)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("scratch created before run: %v", err)
			}
			next, err := scratchDir(home, userHome, tc.goos, "claude", now)
			if err != nil || next == dir {
				t.Fatalf("next=%q error=%v", next, err)
			}
		})
	}
	cfg := config.Config{Agents: map[string]config.AgentConfig{"claude": {Adapter: "claude"}, "fixed": {Adapter: "claude", Dir: "/fixed"}, "codex": {Adapter: "codex"}, "me": {Adapter: "none"}}}
	choose := newThreadDir(cfg, home, userHome, "linux")
	for _, name := range []string{"claude", "fixed", "codex", "me"} {
		dir, err := choose(name)
		if err != nil {
			t.Fatal(err)
		}
		if name == "claude" {
			if !strings.HasPrefix(dir, filepath.Join(home, "work", name)+"/") {
				t.Fatal(dir)
			}
		} else if dir != "" {
			t.Fatalf("%s dir=%s", name, dir)
		}
	}
}
