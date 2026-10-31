# Door probe on grokbot: can Grok's machine be the door?

**Result (2026-10-31): not chosen; the door stays on the iMac.** G1 passed (with
`sudo tailscale set --operator=box`, and a Funnel `nodeAttrs` entry for `tag:grokbot`). Then
grokbot slept about 8 minutes after its last use, and after a ring Funnel came back only after
5 min 26 s, when grokbot reconnected to Tailscale's coordination server (the tailnet was back
after 48 s). ChatGPT could not finish adding the connector. G3–G6 were not reached.

The same probe as [README.md](README.md), run on grokbot instead of the iMac. grokbot sleeps
and is woken by Bridge's doorbell (about 2–2.5 minutes), so besides the three account tests
this asks what sleeping does to a door:

| # | Question | Phase |
|---|---|---|
| G1 | Can grokbot get an HTTPS certificate and run Funnel? | 1, awake |
| G2 | Does the sign-in page on grokbot open from your Mac and phone (and Allow work)? | 1 |
| G3 | Can the Dot send, and answer an event from its text alone, without `fetch_messages`? | 1 |
| G4 | How long does grokbot stay reachable after a Bridge run ends? | 2, sleep |
| G5 | What does ChatGPT do when the door doesn't answer, and does the connector still work after grokbot wakes, without signing in again? | 2 |
| G6 | Can a Dot ring Bridge's webhook itself, wait, then send? | 2 |

| Address | Who can reach it |
|---|---|
| `https://grokbot.example.ts.net/mcp`, `/token`, `/.well-known/…` (Funnel → `127.0.0.1:7790`) | the internet |
| `https://grokbot.example.ts.net:8444/authorize` and `/` | your tailnet only; Allow only from a person's device |

## 0. Policy (admin console, Access controls), before phase 1

Add to `"nodeAttrs"` (Funnel for grokbot; your devices may already have it as `autogroup:member`):

```jsonc
{"target": ["tag:grokbot"], "attr": ["funnel"]},
```

Add to `"grants"` (your devices may open the probe's sign-in page on grokbot; remove after the test):

```jsonc
{"src": ["autogroup:member"], "dst": ["tag:grokbot"], "ip": ["tcp:8444"]},
```

## 1. Phase 1: grokbot awake

The iMac sends the probe to Bridge as a file (`dist/door-probe-linux-amd64`, sha256
`e6287a01a535f623f88ab54fd0b092bd1230bd9c0869b042fefcc8cbdc03e504`). Ask Bridge to run, on
grokbot, and paste back everything printed:

```bash
sha256sum PATH_FROM_BAF_READ
install -m 755 PATH_FROM_BAF_READ ~/.local/bin/door-probe
cd "$(mktemp -d)" && tailscale cert grokbot.example.ts.net && ls -l
tailscale funnel --bg --https=443 http://127.0.0.1:7790
tailscale funnel status
nohup ~/.local/bin/door-probe -public https://grokbot.example.ts.net -signin https://grokbot.example.ts.net:8444 >> ~/.baf-door-probe.out 2>&1 &
sleep 3; tail -5 ~/.baf-door-probe.out
```

**G1 passes** if `tailscale cert` writes two files, `funnel status` shows `https://grokbot…`
proxying to `127.0.0.1:7790`, and the probe logs `door probe: public … sign-in … on 100.64.0.2:8444`.
If `tailscale cert` or `funnel` says *access denied*, Bridge's user isn't Tailscale's
operator; note it (that alone decides a lot) and try once with `sudo`.

Check from outside the tailnet (phone with Tailscale off):
`https://grokbot.example.ts.net/.well-known/oauth-authorization-server` shows JSON, and
`:8444` doesn't load. With Tailscale on, `https://grokbot.example.ts.net:8444/` opens the
probe's page. (A ClashFX-style proxy on the Mac needs an `/etc/hosts` line for
`grokbot.example.ts.net` → `100.64.0.2`, as in README step 2.)

**G2:** in ChatGPT add a custom MCP server **baf door on grokbot**, URL
`https://grokbot.example.ts.net/mcp`, OAuth, as in README step 4. Pass: the Allow page
opens, shows your login, and Bridge's log tail shows `SIGN-IN allowed by "…"`. Try
Allow once from your phone too.

**G3:** ask the Dot:

> Use baf door on grokbot to send "hello from my Dot, via grokbot" to imac/codex.

Then:

> Watch baf door on grokbot for message.created events. Each event's data has the whole
> message in "text". Answer it from that text alone, without calling fetch_messages, by
> sending the answer with send_to_owner to the event's "from".

Open `https://grokbot.example.ts.net:8444/` and select **Send and fire the event**. Pass:
Bridge's log shows `EVENTS delivered … HTTP 2xx`, then `DOT SENT … 399`, and **no**
`fetch_messages` call in between.

## 2. Phase 2: sleep

**G4:** with nothing running, note when Bridge's run ends. From the iMac, every 30 s:
`tailscale ping -c 1 --timeout 5s grokbot`. Record when it stops answering and when
`tailscale status` shows grokbot offline.

**G5:** once grokbot is offline, ask the Dot to send "are you there?" to imac/codex
through the connector. Write down exactly what the Dot and ChatGPT's connector page say.
Then wake grokbot (any `baf send` to `grokbot/bridge` rings Bridge), have Bridge start the
probe again (the `nohup` line), and ask the Dot to send again. Pass: it works without
signing in again. Also watch for ChatGPT asking you to reconnect the connector at any
point.

**G6:** with grokbot offline again, give the Dot Bridge's webhook URL and key (from Grok's
routine settings) and ask:

> POST this JSON to URL with the header "Authorization: Bearer KEY":
> {"baf":1,"from":"openai/dot","wake":"door","messages":[]}
> Then wait 3 minutes and use baf door on grokbot to send "woken by a Dot" to imac/codex.
> If the send fails, wait one more minute and try once more.

Bridge's routine needs one line for this ring: *a ring with `"wake": "door"` names no
messages; start the door probe (the `nohup` line) and keep the run going for 10 minutes.*
Pass: the iMac's log shows nothing (the probe sends nowhere), Bridge's log tail shows
`DOT SENT … woken by a Dot`. Note whether the Dot could make the POST at all, and from
where (its cloud computer, a tool, or not at all).

## 3. Turn it off

On grokbot: `tailscale funnel --https=443 off`, stop the probe (`pkill -f door-probe`),
delete `~/.baf-door-probe`. Remove the `tcp:8444` grant and the connector in ChatGPT.
Give Bridge's routine a new webhook key if the Dot had it.
