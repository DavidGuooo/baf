// Package config reads which agents live on this node and how to run them.
// It holds no permissions: those live only in the Tailscale policy (ADR-0003).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/DavidGuooo/baf/internal/agent"
	"github.com/DavidGuooo/baf/internal/envelope"
)

// Config is ~/.baf/config.json. Every field is optional.
type Config struct {
	// Node overrides this node's name. Normally it comes from Tailscale.
	Node string `json:"node,omitempty"`

	// Agents maps an agent name to how it runs.
	Agents map[string]AgentConfig `json:"agents,omitempty"`
}

// AgentConfig says how to run one agent.
type AgentConfig struct {
	Adapter string `json:"adapter"`           // "codex", "claude", or "none" (inbox only)
	Dir     string `json:"dir,omitempty"`     // working directory; default ~/.baf/work/<agent>
	Bin     string `json:"bin,omitempty"`     // path to the agent's CLI
	Profile string `json:"profile,omitempty"` // codex only: Codex profile (-p)
}

// Default is used when there is no config file: Codex and Claude Code, plus
// "me", an inbox for the owner at a terminal.
func Default() Config {
	return Config{Agents: map[string]AgentConfig{
		"codex":  {Adapter: "codex"},
		"claude": {Adapter: "claude"},
		"me":     {Adapter: "none"},
	}}
}

// Home is baf's directory: $BAF_HOME, or ~/.baf. (Never ~/.tailmsg, which is
// Yobiru's; ADR-0001.)
func Home() (string, error) {
	if h := os.Getenv("BAF_HOME"); h != "" {
		return h, nil
	}
	u, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(u, ".baf"), nil
}

// Load reads home/config.json, or returns Default if there is none.
func Load(home string) (Config, error) {
	raw, err := os.ReadFile(filepath.Join(home, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, fmt.Errorf("config.json: %w", err)
	}
	if len(c.Agents) == 0 {
		c.Agents = Default().Agents
	}
	return c, nil
}

// Adapters builds an adapter for every agent, creating working directories
// as needed. An inbox-only agent maps to nil.
func (c Config) Adapters(home string) (map[string]agent.Adapter, error) {
	out := map[string]agent.Adapter{}
	for _, name := range c.names() {
		if _, err := envelope.ParseAddress("x/" + name); err != nil {
			return nil, fmt.Errorf("agent name %q: %w", name, err)
		}
		ac := c.Agents[name]
		if ac.Adapter == "none" {
			out[name] = nil
			continue
		}
		dir, err := expand(ac.Dir)
		if err != nil {
			return nil, err
		}
		if dir == "" {
			dir = filepath.Join(home, "work", name)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		switch ac.Adapter {
		case "codex":
			out[name] = agent.Codex{Bin: ac.Bin, Dir: dir, Profile: ac.Profile}
		case "claude":
			out[name] = agent.Claude{Bin: ac.Bin, Dir: dir}
		default:
			return nil, fmt.Errorf("agent %q: unknown adapter %q (use codex, claude or none)", name, ac.Adapter)
		}
	}
	return out, nil
}

func (c Config) names() []string {
	var names []string
	for n := range c.Agents {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// expand turns a leading ~ into the user's home directory.
func expand(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	u, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(u, strings.TrimPrefix(p, "~")), nil
}
