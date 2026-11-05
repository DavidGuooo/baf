# Setting up T4: Grok ↔ Codex across machines

T4: a Grok bot on `grokbot` sends a request to `imac/codex`
over the tailnet, and the Result comes back to the bot by ringing the Bridge bot. The code
is done and tested with two mailboxes in one process. These are the steps only you can
take, in order.

## 1. Tailscale policy (admin console)

Paste the parts of [`deploy/tailscale-policy.hujson`](../deploy/tailscale-policy.hujson)
into your policy file under **Access controls**. Tailscale runs the `tests` entry when you
save, so a typo is caught there.

Check it from the iMac (grokbot must be online for its half):

```bash
tailscale whois --json 100.64.0.1 | grep -A3 CapMap
```

## 2. baf on grokbot

Do this from a Grok bot's conversation while grokbot is awake.

1. **Get the binary there.** It is one file with nothing else to install. Run `uname -m`
   on grokbot: `x86_64` needs `baf-linux-amd64`, `aarch64` needs `baf-linux-arm64`. Download
   it from the latest [release](https://github.com/DavidGuooo/baf/releases), check it against
   that release's `SHA256SUMS`, put it at `~/.local/bin/baf` and make it executable.
2. **Give every bot an inbox.** Grok bots are started through Bridge, not by baf, so each
   one only has an inbox, and `"*"` gives one to any bot name. Adding a bot in Grok needs no
   change here; Bridge refuses names that aren't real bots. Write `~/.baf/config.json`:

   ```json
   {"agents": {"*": {"adapter": "none"}}}
   ```
3. **Start it:** `baf start`. It prints `baf started on grokbot`. The log is
   `~/.baf/baf.log`. Nothing keeps it running across a sleep, so Bridge's routine starts it
   each time (step 3).

## 3. Grok: global memory, the Bridge bot and its routine (T0c)

Only **Bridge** runs baf. Other bots never touch it: Bridge brings them each message's
text, and they ask Bridge to send, answer or check a message's status for them. Bridge
sends as the bot it acts for (`--as tokdev`): Tailscale vouches for grokbot, and the agent
name is grokbot's claim, so answers still come back to that bot. Bridge has no memory of
its own.

### Global memory

Add this as one memory (Grok keeps each memory under 500 characters):

```
Baf: messages with the user's agents outside Grok (e.g. openai/dot) via the bot Bridge: give it the recipient (node/agent), if an answer is needed, the body, and optionally a folder on that machine or Claude Desktop (no answer then); ask it for status. Always reply when an answer is needed, even if unfinished (say what's done and left), without asking. Follow-ups are fine. Log each in chat: "Baf: <from> asks me to <summary>. <reasoning>. I respond: <summary>." (omit "I respond" if no reply)
```

### The Bridge bot

The step-by-step part lives in baf itself: `baf guide` prints it, with every command
spelled out. Bridge's instructions and routine only point there, so a new baf release
updates Bridge without anything to paste again.

Create a Grok bot named **Bridge** with these instructions:

```
You are Bridge, my baf endpoint on grokbot. You carry messages between the Grok bots on this computer and my agents outside Grok, for example my ChatGPT Dot (openai/dot).

For any baf work, first run ~/.local/bin/baf guide and follow it exactly. The agents you carry for are the Grok bots here: act as a bot with --as <its name in lowercase>.
```

### Bridge's routine

Give Bridge one routine whose trigger is a **webhook**. Grok shows the webhook URL and its
key; you'll need both in step 4. Paste this as the routine's instructions:

```
Baf has rung: a new message is waiting on grokbot. Run ~/.local/bin/baf guide and follow its WHEN BAF RINGS steps with this webhook's JSON body. The agents are the Grok bots on this computer.
```

Messages may come from a Door agent such as `openai/dot`. The Relay rings Bridge with
that full address as `from`, and Bridge answers it like any other sender:
`baf send openai/dot ...` finds the Relay from the Tailscale policy. Set up the grants in
[Door agents ↔ other machines](public-door.md#door-agents--other-machines) first;
`to` must name `openai/dot` (or `*`) on grokbot's grant toward the Relay.

`baf wait` and `baf flush` each wait up to 10 minutes. grokbot pauses a few minutes after
the last bot run ends, and nothing running inside it (baf included) keeps it awake, so these
waits are what keep it up while a message that failed its first try is retried.

Things to watch for on the first ring (open questions from T0c): does the routine start
with the JSON body intact; does grokbot come online if it was asleep, and how quickly; may
a bot run `baf` without waiting for your approval?

## 4. Give the iMac Bridge's doorbell key

The doorbell key goes only to machines you trust. On the iMac:

```bash
mkdir -p ~/.baf/keys && chmod 700 ~/.baf/keys
pbpaste > ~/.baf/keys/grokbot-bridge && chmod 600 ~/.baf/keys/grokbot-bridge
```

(copy the key first), then add the doorbell to `~/.baf/config.json` on the iMac:

```json
{
  "doorbells": {
    "grokbot": {"url": "<the routine's webhook URL>", "key_file": "~/.baf/keys/grokbot-bridge"}
  }
}
```

Then `baf stop; baf start` on the iMac, and check that the log line `baf is running` shows
`doorbells=grokbot`.

## 5. Run T4

In TokDev's conversation, ask it to have Bridge send `imac/codex` the request
"What is 17*23? Reply with just the number." Bridge runs:

```bash
baf request imac/codex --as tokdev --from-thread tokdev - <<'BAF'
What is 17*23? Reply with just the number.
BAF
baf flush
```

What should happen:

1. The iMac's log shows `push received from_node=grokbot tags=[tag:grokbot] … grants=[{"to":["codex","claude"]}]`.
   That line is the proof the policy block works (the permission arrived through WhoIs).
2. Codex runs (`baf runs` on the iMac).
3. The Result is pushed to grokbot, and the push rings Bridge (`rang doorbell` in the iMac's
   log).
4. Bridge reads the Result as TokDev and brings TokDev the text `391`.

If something goes wrong: `baf outbox` on either machine shows each message's state and
its last error, and a message the receiver refuses comes back as a notice from `<node>/baf`
saying why.

### grokbot doesn't see a policy change

grokbot's computer is paused and resumed, and Tailscale there can come back unable to
reach the coordination server. Messages still flow, but grokbot keeps the policy it had
before the pause, so a grant you just saved (a new `to`, the Relay's `relay: true`)
never arrives; a send to `openai/dot` then fails with `no Relay found in the Tailscale
policy`. Check from a bot's conversation:

```bash
tailscale status --json | python3 -c 'import json,sys; print(json.load(sys.stdin).get("Health"))'
tailscale whois --json 100.64.0.1 | grep -A8 baf/msg
```

If Health says `Unable to connect to the Tailscale coordination server`, restart
Tailscale there as root (from grokbot's own terminal; the bots have no sudo). Its state
lives in `/workspace/tailscale-state`, so it comes back as the same machine:

```bash
sudo sh -c 'pkill -x tailscaled; sleep 2; setsid nohup tailscaled --state=/workspace/tailscale-state/tailscaled.state --socket=/var/run/tailscale/tailscaled.sock --statedir=/workspace/tailscale-state >/var/log/tailscaled.log 2>&1 < /dev/null &'
```

baf needs no restart; it reads the policy each time.
