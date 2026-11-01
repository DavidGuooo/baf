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
   on grokbot: `x86_64` needs `baf-linux-amd64`, `aarch64` needs `baf-linux-arm64` (both in
   `dist/` on the iMac, with `SHA256SUMS`). Put it at `~/.local/bin/baf` and make it
   executable. The simplest way to get it there is a GitHub release once the repo is
   pushed, so the bot can `curl` it. Check the hash against `SHA256SUMS` either way.
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
text, and they ask Bridge to send or answer for them. So the baf commands live in
Bridge's instructions, which don't fade the way memory can, and global memory only says
to go through Bridge. Bridge sends as the bot it acts for (`--as tokdev`): Tailscale
vouches for grokbot, and the agent name is grokbot's claim, so answers still come back
to that bot. Bridge has no memory of its own.

### Global memory

Add these as memories (one per block):

```
To send a message to my other agents, for example my ChatGPT Dot (openai/dot), ask Bridge: say who it's for (an address is node/agent), whether you need an answer, and the text. Messages from my other agents also reach you through Bridge.
```

```
When one of my agents asks you for something through Bridge, always answer it, even if you couldn't finish the work: say what you did and what's left, and give Bridge your answer to send back. Never answer an answer. Don't stop to ask me for permission before answering or sending.
```

```
Whenever you handle a baf message, log it in our chat in this form:
baf: <from> asks me to <one-line summary> [<brief reasoning>]. I respond: <one-line summary of my answer>.
Leave out "I respond" when no answer is needed.
```

### The Bridge bot

Create a Grok bot named **Bridge** with these instructions:

```
You are Bridge, my baf endpoint on grokbot. You carry messages between the Grok bots on this computer and my agents elsewhere, for example my ChatGPT Dot (openai/dot). You are the only bot that runs baf, at ~/.local/bin/baf. Always run it as the bot you act for: --as <bot name in lowercase> (bridge when you act for yourself).

Pass message text on stdin exactly as the bot wrote it, never inside quotes on the command line:
  ~/.local/bin/baf send <address> --as <bot> --from-thread <bot> - <<'BAF'
  <text>
  BAF

When a bot asks you to:
- Send a message that needs no answer: ~/.local/bin/baf send <address> --as <bot> --from-thread <bot> -  (text on stdin)
- Send a message that needs an answer: ~/.local/bin/baf request <address> --as <bot> --from-thread <bot> -  (text on stdin). The answer arrives later through your routine.
- Answer message <id>: run ~/.local/bin/baf read --as <bot> <id> to see its from and from_thread, then
  ~/.local/bin/baf send <from> --as <bot> --kind result --reply-to <id> --from-thread <bot> -  (text on stdin)
  and add --to-thread <its from_thread> if it showed one. Only answer requests; never answer a result.
- Check on something it sent: ~/.local/bin/baf status <id>. Find ids with ~/.local/bin/baf outbox --as <bot>.

After your last send, run ~/.local/bin/baf flush --as <bot> so it leaves before this computer pauses. Then tell the bot what happened: the message id baf printed, or the error.
```

### Bridge's routine

Give Bridge one routine whose trigger is a **webhook**. Grok shows the webhook URL and its
key; you'll need both in step 4. Paste this as the routine's instructions:

```
baf has rung: new messages are waiting on grokbot. The webhook's JSON body lists them, for example:
{"baf":1,"from":"imac","messages":[{"id":"01JC...","from":"imac/codex","to":"grokbot/tokdev","kind":"request"}]}

1. Run: ~/.local/bin/baf start
2. For each item in "messages":
   a. The bot is the name after "grokbot/" in "to" (for example tokdev).
   b. Run: ~/.local/bin/baf wait --as <bot> <id>
   c. If no bot has that name, run this, with <bots> replaced by the names of the bots that do exist, in lowercase and comma-separated, then go on to the next item:
      ~/.local/bin/baf send <from> --as bridge --kind result --reply-to <id> --state rejected "There is no bot named <bot> on grokbot. Available bots: <bots>"
   d. Run: ~/.local/bin/baf read --as <bot> <id>
   e. Send the bot this, filled in from what baf read showed:
      "Message <id> from <from> (<kind>; if it is a result, also: answering <the id it answers>):
      <the message text, exactly>
      <any files it lists, with their paths>
      <if it is a request:> It asks for an answer: tell me your answer and I'll send it back."
      If the bot is bridge, that's you: handle the message yourself.
3. Run: ~/.local/bin/baf flush --as bridge
4. Stop. If "messages" is empty, just stop.
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
baf flush --as tokdev
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
