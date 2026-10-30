# baf

Messages between your personal agents (Codex, Claude Code, Grok Bot, …) over your
Tailscale network. Each agent's mailbox lives on its own machine; Tailscale says who is
connecting and what they may do. Design: [`docs/design-notes.md`](docs/design-notes.md),
vocabulary: [`GLOSSARY.md`](GLOSSARY.md), decisions: [`docs/adr/`](docs/adr/).

**Status:** the one-node core works: agents on the same machine can send each other notices
and requests, and every request gets a Result back automatically. Sending between machines
comes next (design notes section 8, T4).

## Build

Needs Go 1.27 or newer.

```bash
go build -o baf ./cmd/baf
```

## Try it

Start the mailbox (keep it running; it uses `~/.baf`, or `$BAF_HOME`):

```bash
./baf serve
```

In another terminal, ask Codex something. You are the agent `me`:

```bash
./baf request codex "What is 17*23? Reply with just the number."
./baf inbox
./baf read <result id>
```

Continue the same Codex thread by passing the thread id the Result came from:

```bash
./baf request codex --to-thread <from_thread of the result> "And times 2?"
```

`./baf runs` lists the agent runs and how they ended.

## Agents

Without a config file, this machine has `codex`, `claude` (Claude Code) and `me` (an inbox
for you). To change that, write `~/.baf/config.json`:

```json
{
  "agents": {
    "codex":  {"adapter": "codex", "dir": "~/code/sandbox", "profile": "baf"},
    "claude": {"adapter": "claude", "dir": "~/code/sandbox"},
    "me":     {"adapter": "none"}
  }
}
```

`dir` is where the agent works (default `~/.baf/work/<agent>`). Requests run in each
agent's own auto mode, so nothing waits for a permission prompt: Codex with
`--approve-for-me` (workspace-write sandbox), Claude Code with `--permission-mode auto
--permission-prompts none`.

## Develop

```bash
go test -race ./...
```
