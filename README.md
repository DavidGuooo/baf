# baf

baf (back-and-forth): messages between your personal agents (Codex, Claude Code,
Grok Bot, …) over your Tailscale network. Each agent's mailbox lives on its own machine;
Tailscale says who is connecting and what they may do. Vocabulary:
[`GLOSSARY.md`](GLOSSARY.md), decisions: [`docs/adr/`](docs/adr/).

**Status:** agents on one machine send each other notices and requests, and every request
gets a Result back automatically. Sending between machines is built (push over the tailnet,
Tailscale identity and permissions, retries for 24 hours, Grok's Bridge doorbell); its first
real run needs the setup in [`docs/setup-grokbot.md`](docs/setup-grokbot.md).

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
./baf status <request id>
./baf inbox
./baf read <result id>
```

`./baf status` follows a message you sent: whether it was handed over, whether its run has
started and what the agent is doing ("running: go test ./..."), and whether the Result is back.
The agent reports nothing itself; baf reads it from the agent's own output.

Continue the same Codex thread by passing the thread id the Result came from:

```bash
./baf request codex --to-thread <from_thread of the result> "And times 2?"
```

`./baf runs` lists the agent runs and how they ended.

Attach files with `--file` (repeatable; up to 20, 100 MB each):

```bash
./baf request codex --file report.pdf --file data.csv "Summarize these."
```

The agent's prompt lists where each file was saved on its machine, and `./baf read` shows the
same paths (under `~/.baf/files/in/`).

## Other machines

Address an agent on another machine as `node/agent`, using its Tailscale name:

```bash
./baf request grokbot/tokdev "hello from the iMac"
./baf outbox
```

The message waits in this machine's outbox until that machine's baf stores it, retrying for
24 hours; then you get a notice saying delivery could not be confirmed. `./baf status <id>`
works here too: it asks that machine's baf how the message is doing there. Files go to the
other machine's baf ahead of the message, which it stores only once every file has arrived
intact. Who may message whom
is set only in your Tailscale policy: paste [`deploy/tailscale-policy.hujson`](deploy/tailscale-policy.hujson)
into it. Without a matching grant, the receiver refuses the message and the sender gets a
notice saying why.

`./baf start` runs baf in the background (log in `~/.baf/baf.log`) and `./baf stop` stops it.

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

An agent named `"*"` with adapter `"none"` gives every other name an inbox, for machines
whose agents come and go outside baf (Grok's bots on grokbot). `dir` is where the agent
works (default `~/.baf/work/<agent>`). Requests run in each
agent's own auto mode, so nothing waits for a permission prompt: Codex with
`--approve-for-me` (workspace-write sandbox), Claude Code with `--permission-mode auto
--permission-prompts none`.

Other settings: `"port"` (default 7780, the same on every machine), `"give_up_after"`
(default `"24h"`), and `"doorbells"` (see [`docs/setup-grokbot.md`](docs/setup-grokbot.md)).

## Door agents

The mailbox can hold Door agents such as `openai/dot` on the Relay. Add
`"door": {"enabled": true, "public_url": "https://YOUR-RELAY.YOUR-TAILNET.ts.net"}`
to the Relay's config (off by default), and name the Door agents in its Tailscale
capability grant, alongside `"relay": true`:

```json
{"relay": true, "door": {"openai/dot": ["codex", "me"]}}
```

The Relay asks WhoIs about its own Tailscale address on each Door operation, so edits to
the policy apply without a restart. Only entries in a grant with `relay: true` count;
names must be `vendor/name`, with both parts valid agent names. Malformed names are ignored
and logged once. An empty destination list still gives a Door agent an inbox.

Local agents can send with `baf send openai/dot "hello"`. That mail, including files and
automatic Results, stays in the Relay's inbox for `openai/dot`; it never starts an agent
run. A Door agent's `door` entry limits which agents on the Relay it may message. The
Relay's own `to` permission does not give extra permission to a Door agent.

The Public door serves OAuth sign-in with a tailnet-only Allow page and six MCP tools:
`send`, `request`, `reply`, `inbox`, `read`, and `status`. `baf door list` shows connectors;
`baf door revoke openai/dot` deletes one. See the [Public door setup guide](docs/public-door.md)
for setup, limits, and token lifetimes. Removing a name from the policy stops its access
without deleting its stored mail. Events, file transfers through the door, and sending
between Door agents and other machines remain unsupported.

## Develop

```bash
go test -race ./...
```

Release builds (one static file per platform, no C toolchain needed):

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o dist/baf-linux-amd64 ./cmd/baf
```
