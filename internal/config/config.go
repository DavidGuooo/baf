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
	"time"

	"github.com/DavidGuooo/baf/internal/agent"
	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/DavidGuooo/baf/internal/mailbox"
	"github.com/DavidGuooo/baf/internal/transport"
)

// Config is ~/.baf/config.json. Every field is optional.
type Config struct {
	// Node overrides this node's name. Normally it comes from Tailscale.
	Node string `json:"node,omitempty"`

	// Agents maps an agent name to how it runs.
	Agents map[string]AgentConfig `json:"agents,omitempty"`

	// Port is baf's port on every node (default 7780).
	Port int `json:"port,omitempty"`

	// GiveUpAfter is how long to keep trying to hand a message to another
	// node, as a Go duration such as "24h" (the default).
	GiveUpAfter string `json:"give_up_after,omitempty"`

	// Doorbells are the doorbell keys other nodes' owners gave this node,
	// by node name. For grokbot it is the Bridge bot's routine webhook.
	Doorbells map[string]DoorbellConfig `json:"doorbells,omitempty"`
}

// DoorbellConfig is one node's doorbell. The key stays in its own file
// (readable only by you), so config.json holds no secrets.
type DoorbellConfig struct {
	URL     string `json:"url"`
	KeyFile string `json:"key_file"`
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
	if c.GiveUpAfter != "" {
		if _, err := time.ParseDuration(c.GiveUpAfter); err != nil {
			return Config{}, fmt.Errorf("config.json give_up_after: %w", err)
		}
	}
	return c, nil
}

// GiveUp returns GiveUpAfter as a duration (0 means the default).
func (c Config) GiveUp() time.Duration {
	d, _ := time.ParseDuration(c.GiveUpAfter)
	return d
}

// Bells reads each doorbell's key from its file.
func (c Config) Bells() (map[string]transport.Bell, error) {
	out := map[string]transport.Bell{}
	for node, d := range c.Doorbells {
		path, err := expand(d.KeyFile)
		if err != nil {
			return nil, err
		}
		key, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("doorbell key for %s: %w", node, err)
		}
		if d.URL == "" {
			return nil, fmt.Errorf("doorbell for %s has no url", node)
		}
		out[node] = transport.Bell{URL: d.URL, Key: strings.TrimSpace(string(key))}
	}
	return out, nil
}

// Adapters builds an adapter for every agent, creating working directories
// as needed. An inbox-only agent maps to nil.
func (c Config) Adapters(home string) (map[string]agent.Adapter, error) {
	out := map[string]agent.Adapter{}
	for _, name := range c.names() {
		ac := c.Agents[name]
		if name == mailbox.AnyAgent {
			// "*": every other agent name gets an inbox (see mailbox.AnyAgent).
			if ac.Adapter != "none" {
				return nil, fmt.Errorf(`agent "*" must use adapter "none": it gives any name an inbox, it can't run one`)
			}
			out[name] = nil
			continue
		}
		if _, err := envelope.ParseAddress("x/" + name); err != nil {
			return nil, fmt.Errorf("agent name %q: %w", name, err)
		}
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
		// The agent's process starts in dir and Codex is also told -C dir, so
		// a relative dir (from a relative BAF_HOME) would apply twice.
		if dir, err = filepath.Abs(dir); err != nil {
			return nil, err
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
