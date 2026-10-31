package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDoorConfig(t *testing.T) {
	home := t.TempDir()
	cfg, err := Load(home)
	if err != nil || cfg.Door.Enabled {
		t.Fatalf("default door = %+v, err = %v", cfg.Door, err)
	}
	for _, tc := range []struct {
		raw     string
		enabled bool
	}{
		{`{}`, false},
		{`{"door":{}}`, false},
		{`{"door":{"enabled":false}}`, false},
		{`{"door":{"enabled":true}}`, true},
	} {
		if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(tc.raw), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(home)
		if err != nil || cfg.Door.Enabled != tc.enabled || len(cfg.Agents) == 0 {
			t.Fatalf("%s: cfg = %+v, err = %v", tc.raw, cfg, err)
		}
	}
}
