# Door probe: can a ChatGPT Dot use baf's public door?

A throwaway test server, not part of baf. It answers the account tests from design notes
section 9 (*Agents that can't join the tailnet*) before the real door is built:

1. **Can a Dot send?** ChatGPT calls the `send_to_owner` tool (a write action).
2. **Does an MCP event start a Dot task?** The Dot subscribes to `message.created`; you
   send it a message from a web page and see whether the Dot wakes up and answers.
3. **Does ChatGPT accept a sign-in page that only opens from your tailnet?** The "Allow?"
   page listens only on the iMac's Tailscale address; everything ChatGPT's servers call is
   on Funnel.

Messages go nowhere: the probe stores them and logs every request ChatGPT makes, with
tokens and secrets cut to their first six characters. Log and state:
`~/.baf-door-probe/door.log` and `state.json`.

| Address | Through | Who can reach it |
|---|---|---|
| `https://imac.example.ts.net/mcp`, `/token`, `/register`, `/.well-known/…` | Funnel, port 443 → `127.0.0.1:7790` | the internet (ChatGPT's servers) |
| `https://imac.example.ts.net:8444/authorize` and `/` | the probe itself on `100.64.0.1:8444`, with Tailscale's certificate | only your tailnet; **Allow** works only from a person's device, never a tagged one like grokbot |

Not Tailscale Serve for the sign-in page: on macOS a machine can't reach its own Serve
port, and you may approve from the iMac itself.

## 1. Switch on HTTPS and Funnel (admin console, once)

- **HTTPS:** open the link `tailscale serve` prints (it was
  `https://login.tailscale.com/f/serve?node=niLPW7Lij121CNTRL`) and enable it. This turns on
  HTTPS certificates for the tailnet.
- **Funnel:** the funnel command in step 3 prints a similar link if Funnel isn't allowed
  yet. (By hand: under **Access controls**,
  `"nodeAttrs": [{"target": ["autogroup:member"], "attr": ["funnel"]}]`.)

## 2. On a Mac with ClashFX (or another proxy in TUN mode)

ClashFX points the Mac's DNS at its own fake address (`198.18.0.2`). Two things break:

- **Tailscale can't get its certificate.** Its own lookups go out the network card, where
  `198.18.0.2` doesn't answer (`tailscale cert` fails with
  `lookup acme-v02.api.letsencrypt.org: i/o timeout`). Quit ClashFX for a minute, run this,
  then start ClashFX again. Tailscale keeps the certificate (about 90 days) and uses it for
  Funnel and the probe.

  ```bash
  cd "$(mktemp -d)" && tailscale cert imac.example.ts.net
  ```

- **Browsers can't find `imac.example.ts.net`.** ClashFX answers with a fake address
  and sends the request to the proxy server, which doesn't know the name. Add the real
  address to `/etc/hosts`, which the Mac reads before asking DNS (remove the line when
  done):

  ```bash
  echo "100.64.0.1 imac.example.ts.net" | sudo tee -a /etc/hosts
  ```

## 3. Start the probe and publish it

From the repo, in a terminal you leave open:

```bash
go run ./probes/door -public https://imac.example.ts.net -signin https://imac.example.ts.net:8444
```

```bash
tailscale funnel --bg --https=443 http://127.0.0.1:7790
```

Check: `https://imac.example.ts.net:8444/` opens the probe's page on a tailnet
device, and from outside the tailnet (phone with Tailscale off)
`https://imac.example.ts.net/.well-known/oauth-authorization-server` shows JSON while
the `:8444` page doesn't load. A brand-new Funnel name can take a few minutes to appear in
public DNS.

## 4. Add it to ChatGPT

1. Go to <https://chatgpt.com/plugins>, select **+**, then **Add custom MCP server**.
2. Name **baf door probe**; server URL `https://imac.example.ts.net/mcp`;
   authentication **OAuth** (keep the default registration method; the probe accepts both
   CIMD and dynamic registration).
3. Accept the risk warning, select **Create as a plugin**, then install it from your
   personal plugins.
4. When ChatGPT asks you to sign in, the probe's **Allow?** page should open, showing your
   Tailscale login. Select **Allow**. (**Test 3 passes** if this works and the log shows
   `SIGN-IN allowed by "<your login>"`.)
5. The plugin page should list two tools and the `message.created` event. If the event is
   missing, ChatGPT didn't use MCP 2.0 (the log shows `initialize` but no `server/discover`).

## 5. Run the tests from a Dot

**Test 1, sending:** ask your Dot:

> Use baf door probe to send "hello from my Dot" to imac/codex.

Pass: the log shows `DOT SENT … to imac/codex`. Note whether ChatGPT asked you to
approve the write first.

**Test 2, the event:** ask your Dot:

> Watch baf door probe for message.created events. When one arrives, fetch the message with
> fetch_messages and answer its question by sending the answer with send_to_owner to the
> message's sender.

Pass so far: the log shows `EVENTS verification reply 200` and `EVENTS subscribed`. Then
open `https://imac.example.ts.net:8444/` and select **Send and fire the event**
(it asks 19×21). Pass: `EVENTS delivered … HTTP 2xx`, then a new `DOT SENT` with `399`,
with no message from you in between. Note the time from send to answer.

## 6. Turn it off

```bash
tailscale funnel --https=443 off
```

Stop the probe (Ctrl-C), and remove the plugin in ChatGPT. Delete `~/.baf-door-probe`
and the `/etc/hosts` line when the results are written down.
