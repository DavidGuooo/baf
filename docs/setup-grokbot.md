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

## 3. The Bridge bot and its routine (T0c)

Create a Grok bot named **Bridge** with one routine whose trigger is a **webhook**. Grok
shows the webhook URL and its key; you'll need both in step 4. Paste this as the routine's
instructions:

```
You are Bridge. When baf rings this routine, you hand each new baf message to the bot it is addressed to. You never open or read the messages yourself. baf is installed at ~/.local/bin/baf.

The webhook's JSON body lists the messages, for example:
{"baf":1,"from":"imac","messages":[{"id":"01JC...","from":"imac/codex","to":"grokbot/tokdev","kind":"request"}]}

Do this:
1. Run: ~/.local/bin/baf start
2. For each item in "messages":
   a. The bot is the name after "grokbot/" in "to" (for example tokdev).
   b. Run: ~/.local/bin/baf wait --as <bot> <id>
   c. If that bot exists, send it this message, with <bot>, <id> and <from> filled in:
      "A baf message is waiting for you: <id> from <from>. Read it with:
         ~/.local/bin/baf read --as <bot> <id>
       If it is a request, send your answer back with:
         ~/.local/bin/baf send <from> --as <bot> --kind result --reply-to <id> --from-thread <bot> "your answer"
       If the message shows a from_thread, also add: --to-thread <that from_thread>
       After your last baf send, run this before you finish, so your messages leave before this computer pauses:
         ~/.local/bin/baf flush --as <bot>"
   d. If no bot has that name, run this, with <bots> replaced by the names of the bots that do exist, in lowercase and comma-separated:
      ~/.local/bin/baf send <from> --as bridge --kind result --reply-to <id> --state rejected "There is no bot named <bot> on grokbot. Available bots: <bots>"
3. Run: ~/.local/bin/baf flush --as bridge
4. Stop. If "messages" is empty, just stop.
```

Messages may also come from a Door agent such as `openai/dot`. The Relay rings
Bridge with that full address in both the top-level `from` and the message's `from`.
The routine above needs no change: it uses each message's `from` when telling the
bot where to answer. `baf send openai/dot ...` finds the Relay from the Tailscale
policy and hands the answer to its Door inbox. Keep `baf flush` so the answer leaves
before grokbot pauses. Set up the grants in
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

From a Grok bot's conversation (TokDev here):

```bash
baf start
baf request imac/codex --as tokdev --from-thread tokdev "What is 17*23? Reply with just the number."
baf outbox --all
```

What should happen:

1. The iMac's log shows `push received from_node=grokbot tags=[tag:grokbot] … grants=[{"to":["codex","claude"]}]`.
   That line is the proof the policy block works (the permission arrived through WhoIs).
2. Codex runs (`baf runs` on the iMac).
3. The Result is pushed to grokbot, and the push rings Bridge (`rang doorbell` in the iMac's
   log).
4. Bridge hands the message id to TokDev, which reads `391` with `baf read`.

If something goes wrong: `baf outbox --all` on either machine shows each message's state and
its last error, and a message the receiver refuses comes back as a notice from `<node>/baf`
saying why.
