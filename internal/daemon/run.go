package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/DavidGuooo/baf/internal/config"
	"github.com/DavidGuooo/baf/internal/runfolder"
)

func newThreadDir(cfg config.Config, home, userHome, goos string) func(string) (string, error) {
	return func(name string) (string, error) {
		ac := cfg.Agents[name]
		if ac.Adapter != "claude" || ac.Dir != "" {
			return "", nil
		}
		return scratchDir(home, userHome, goos, name, time.Now())
	}
}

func scratchDir(home, userHome, goos, name string, now time.Time) (string, error) {
	root := filepath.Join(home, "work", name)
	if goos == "darwin" {
		var settings struct {
			Account struct {
				AccountUUID      string `json:"accountUuid"`
				OrganizationUUID string `json:"organizationUuid"`
			} `json:"oauthAccount"`
		}
		data, err := os.ReadFile(filepath.Join(userHome, ".claude.json"))
		if err == nil && json.Unmarshal(data, &settings) == nil {
			account, org := settings.Account.AccountUUID, settings.Account.OrganizationUUID
			if accountPart(account) && accountPart(org) {
				root = filepath.Join(userHome, "Library", "Application Support", "Claude", "scratch-workspaces", account, org)
			}
		}
	}
	var suffix [3]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	path := filepath.Join(root, "scratch-"+now.UTC().Format("2006-01-02")+"-"+hex.EncodeToString(suffix[:]))
	return runfolder.Resolve(path, userHome)
}

// Account identifiers must stay within the Desktop scratch layout.
func accountPart(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\\x00")
}
