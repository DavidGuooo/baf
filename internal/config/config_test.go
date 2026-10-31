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

func TestDoorValidation(t *testing.T) {
	d := DoorConfig{Enabled: true, PublicURL: "https://relay.example.ts.net/"}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if d.PublicURL != "https://relay.example.ts.net" || d.SigninPort != 8444 || d.PublicListen != "127.0.0.1:7790" {
		t.Fatalf("defaults: %+v", d)
	}
	for _, raw := range []string{"", "http://relay.example.ts.net", "https://relay.example.ts.net/path", "https://relay.example.ts.net?key=value", "https://owner@relay.example.ts.net", "https://relay.example.ts.net:8444", "https://127.0.0.1", "https://relay.example.ts.net/#fragment"} {
		d := DoorConfig{Enabled: true, PublicURL: raw}
		if d.Validate() == nil {
			t.Errorf("accepted public_url %q", raw)
		}
	}
	for _, port := range []int{-1, 443, 65536} {
		d := DoorConfig{Enabled: true, PublicURL: "https://relay.example.ts.net", SigninPort: port}
		if d.Validate() == nil {
			t.Errorf("accepted signin_port %d", port)
		}
	}
	for _, addr := range []string{"0.0.0.0:7790", ":7790", "[::]:7790", "100.1.2.3:7790", "localhost:7790", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:abc"} {
		d := DoorConfig{Enabled: true, PublicURL: "https://relay.example.ts.net", PublicListen: addr}
		if d.Validate() == nil {
			t.Errorf("accepted public_listen %q", addr)
		}
	}
	d = DoorConfig{Enabled: false, PublicURL: "unused", SigninPort: -1}
	if err := d.Validate(); err != nil {
		t.Fatal("disabled door:", err)
	}
}
