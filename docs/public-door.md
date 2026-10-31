# Setting up the Public door: ChatGPT Dot ↔ baf agents

The Public door lets a ChatGPT Dot use baf as `openai/dot`. The Relay holds its mail,
checks its permissions in the Tailscale policy, and exposes six MCP tools through
Funnel. Agents on the Relay and other tailnet machines can exchange mail with it. MCP
events ring the Dot when new mail arrives; `inbox` stays the fallback. File transfers
through MCP are not implemented yet.

These are steps for the owner to run. In the examples, the Relay is **imac**, its
DNS name is **`imac.example.ts.net`**, and its Tailscale IPv4 address is
**`100.64.0.1`**. These are examples, not required names or addresses: replace them
with your Relay's values from `tailscale status --json` and `tailscale ip -4`.

## 1. Tailscale policy (admin console)

Under **Access controls**, put `relay: true` and `door` in the baf capability grant
for **imac itself**. The Relay reads its own grant with WhoIs, so this must be a
grant that applies when imac asks about its own address. For example, add this
entry to `grants`, using your Relay's address for both `src` and `dst`:

```json
{
  "src": ["100.64.0.1"],
  "dst": ["100.64.0.1"],
  "ip": ["tcp:7780"],
  "app": {
    "github.com/DavidGuooo/baf/msg": [{
      "to": ["*"],
      "relay": true,
      "door": {"openai/dot": ["codex", "claude", "me"]}
    }]
  }
}
```

The `door` entry permits `openai/dot` to message those three agents on the Relay.
It does not create the local agents: they must already exist in baf's `agents` config.
An empty list gives the Door agent an inbox but no permission to send. Permissions
from matching grants add together; adding a narrower grant does not remove an older
`"*"` permission.

Who may message `openai/dot` is expressed in ordinary `to` lists on grants to the
Relay: name `"openai/dot"`, or use `"*"` to include it. Local agents on the Relay use
the owner-only baf socket and can already send to it, like any local inbox; they
share the node's trust. Messages from other machines go through the Relay, as described
in **Door agents ↔ other machines** below.
The Relay's `to` list never expands what a Door agent's own `door` entry permits.

Allow the owner's devices to reach the sign-in port. For example, add to `grants`:

```json
{
  "src": ["autogroup:member"],
  "dst": ["100.64.0.1"],
  "ip": ["tcp:8444"]
}
```

Check the capability on the Relay:

```bash
tailscale whois --json 100.64.0.1
```

Its `CapMap` should include `github.com/DavidGuooo/baf/msg` with `relay` and `door`.
The door checks the current policy on sign-in, token exchange, and mailbox operations.
Changing the destinations needs no new sign-in or baf restart.
See [ADR-0008](adr/0008-outside-agents-enter-by-the-door-under-the-policy.md).

## 2. HTTPS, Funnel permission, and ClashFX

Enable HTTPS certificates under **DNS** in the Tailscale admin console, with MagicDNS
on. Enable Funnel for the Relay in the policy's `nodeAttrs`, for example:

```json
{"target": ["100.64.0.1"], "attr": ["funnel"]}
```

Tailscale documents [HTTPS setup](https://tailscale.com/docs/how-to/set-up-https-certificates)
and [Funnel](https://tailscale.com/docs/features/tailscale-funnel). The Funnel command
in step 4 also offers an enablement link when needed.

On a Mac with ClashFX in TUN mode, two separate problems occurred in the probe:

- **Getting the certificate:** Tailscale's DNS lookup reached ClashFX's fake DNS address
  (`198.18.0.2`) through the network card and timed out. Quit ClashFX, fetch the certificate
  once with the proxy off, then start ClashFX again:

  ```bash
  (umask 077; cd "$(mktemp -d)" && tailscale cert imac.example.ts.net)
  ```

  Tailscale caches the certificate; baf gets it through Tailscale's local API. The
  exported files in that temporary directory are not baf config files. If certificate
  renewal later fails with the same DNS timeout, check the proxy again.
- **Opening the Allow page:** ClashFX may resolve the Relay's `ts.net` name to a fake
  address and send it to a proxy that cannot reach the tailnet. Add the real mapping
  to `/etc/hosts` on the Mac used to approve (only once; replace these example values):

  ```bash
  echo "100.64.0.1 imac.example.ts.net" | sudo tee -a /etc/hosts
  ```

  Keep that line while this workaround is needed, and remove it if the Relay's address
  changes or you stop using the door. Other approving devices need working tailnet DNS too.

## 3. Configure baf and keep the Relay awake

Merge this into `~/.baf/config.json` on the Relay, keeping its existing settings:

```json
{
  "door": {
    "enabled": true,
    "public_url": "https://imac.example.ts.net",
    "signin_port": 8444,
    "public_listen": "127.0.0.1:7790"
  }
}
```

The door is disabled by default. `public_url` must be this Relay's Tailscale DNS name,
using HTTPS on port 443 with no path. The other two settings shown are the defaults.
`public_listen` must be a loopback IP and port. `signin_port` must differ from 443 and
baf's mailbox port (normally 7780). Invalid settings stop startup with an error.

Keep this Mac from sleeping, then restart baf to load the configuration:

```bash
sudo pmset -a sleep 0
baf stop
baf start
```

The Relay must stay online: waking a sleeping machine does not guarantee Funnel is
immediately reachable. The sign-in listener binds only the Relay's Tailscale addresses.
It serves HTTPS itself using Tailscale's certificate; it does not use Tailscale Serve,
since a Mac cannot reach its own Serve port. The public listener binds only loopback.

## 4. Publish the public listener

Forward Funnel's HTTPS port 443 to the configured `public_listen`:

```bash
tailscale funnel --bg --https=443 http://127.0.0.1:7790
```

baf does not configure Funnel for you. If that port still serves the throwaway probe,
stop the probe before starting baf; both cannot bind `127.0.0.1:7790`.

From outside the tailnet, this example URL should return OAuth metadata:

```text
https://imac.example.ts.net/.well-known/oauth-authorization-server
```

The `authorization_endpoint` should point to the same name at **`:8444/authorize`**.
That port should open only from a tailnet device. Visiting `/authorize` without the
OAuth query gives an error; the connector supplies the full link. Never publish
`:8444` through Funnel. A new Funnel name may take a few minutes to reach public DNS.

## 5. Add the connector in ChatGPT

The probe used this flow; labels may differ by account:

1. Open <https://chatgpt.com/plugins>, select **+**, then **Add custom MCP server**.
2. Name it **baf**. Use the URL below and select **OAuth**. Keep the default registration
   method; baf accepts both HTTPS client metadata documents (CIMD) and dynamic registration.

   ```text
   https://imac.example.ts.net/door/openai/dot/mcp
   ```

3. Create the plugin and install it from your personal plugins.
4. When prompted to sign in, open the Allow page from a person's device connected to
   the tailnet. It shows your Tailscale login, the client, and its redirect address.
   Tagged devices cannot approve. Select **Allow**.
5. Check that the connector lists `send`, `request`, `reply`, `inbox`, `read`, and `status`.
   It should also list `message.created`. A second Door agent needs its own policy entry
   and connector URL.

A sign-in replaces the earlier connector for that Door agent immediately. For `openai`
names, the Allow page warns about client or redirect URLs outside `chatgpt.com` /
`openai.com` and their subdomains. The warning does not grant extra permissions.

## 6. Try the tools

Ask the Dot to use baf to **send** a notice to `me`. On the Relay:

```bash
baf inbox --as me
baf read --as me <message-id>
```

The Principal should be `openai/dot`. `me` and `imac/me` mean the same destination
in this example. A destination on another machine is checked by that machine against
the Relay's grant toward it, as described below.

Ask the Dot to **request** an answer from `codex`. It gets a receipt immediately; when
Codex's run finishes, the Result appears in the Dot's **inbox**. **status** reports
progress and the Result id. Watch for events (below) to receive the whole Result, or
check inbox for answers.

To ask the Dot something from the Relay:

```bash
baf request openai/dot --as me "What is 19 times 21?"
```

Ask the Dot to check **inbox**, **read** the request, then **reply** with its id in
`reply_to`, the answer in `text`, and `state: "completed"`. `failed` and `rejected` are
also supported; the default is `completed`. The recipient comes from the stored
request, and `to_thread` defaults to that request's `from_thread`. `from_thread` is
optional; an explicit `to_thread` must have been shared by the recipient.

**inbox** lists unread mail without changing it; `all: true` includes read mail.
**read** marks a message read without deleting it. The sender can see that it was
opened with `baf status <message-id>`. A receipt means stored, not read or completed.

Text is limited to 1 MiB of decoded UTF-8. The MCP request body is limited to 1 MiB
plus 16 KiB for its wrapper; JSON escaping counts toward that body limit. OAuth
requests, request URLs, and configured HTTP headers are limited to 16 KiB.
`send`, `request`, `reply`, and `read` share a limit of **30 attempts in any rolling
minute per Door agent**. Validated attempts that fail in the mailbox count too.
`inbox` and `status` are read-only and do not consume this limit. Token renewal or
replacement does not reset it; restarting baf does. Over-limit calls return a tool
error and make no mailbox change.

Files cannot be uploaded or downloaded through these tools. Mail already containing
files may list their names and sizes, but the door never returns local file paths.

## 7. Watch for events

Ask the Dot:

> Watch baf for message.created events. Check inbox first for any waiting mail. When an
> event arrives, use its full message in data. For a Request, answer with reply: put
> data.id in reply_to, your answer in text, and completed in state. Use the message id
> to avoid answering twice. Use inbox/read if an event is missing. Do not reply to Results.

Then, on the Relay, send a new request:

```bash
baf request openai/dot --as me "What is 19 times 21?"
```

With `baf start`, look in `~/.baf/baf.log` (`baf serve` prints to its terminal):

```bash
tail -f ~/.baf/baf.log | grep 'door events'
```

`door events subscribed` means the HTTPS callback passed the signed verification
challenge. `door events delivered` with `status=200` (or another 2xx) means the callback
accepted an event. Check `baf inbox --as me` for the Dot's Result, ideally without another
prompt from you. Callback acceptance alone does not prove the Dot started or finished.
`door events verification failed` means there is no new active subscription;
`door events retry` means a delivery failed and is waiting to try again. Logs name the
Door agent and event/subscription ids, never callback URLs, secrets, or message text.

The event's `data` contains `id`, `from` (the Principal), `to`, `kind`, the complete
`text`, `from_thread`, `to_thread`, `reply_to`, and `state` (the Result's outcome, empty
otherwise). File metadata is included when present, without local paths or file bytes.
An event is a wake: it does not mark mail read, change its state, or confirm a handover.
Use `read` explicitly when you want the sender to see that the message was opened.

Subscriptions last **24 hours by default** when ChatGPT omits `ttlMs`. Explicit durations
are clamped to one minute through seven days; `ttlMs: null` grants seven days.
The response's `refreshBefore` tells the client when to
renew with `events/subscribe`. Renewals verify the callback again. `events/unsubscribe`
uses the same event name, empty arguments, and webhook delivery URL as subscribe.
Only public HTTPS callbacks are accepted; redirects are never followed, and each
callback attempt has a ten-second timeout. Each Door agent's subscriptions are separate.

Subscriptions and pending events survive a restart. Failed deliveries retry after
2, 4, 8 seconds and so on, capped at five minutes, for up to **one hour from storage**.
After that baf quietly drops the event; the mail still waits in inbox. HTTP 410 removes
the subscription; HTTP 413 drops that event. A crash after callback acceptance can
repeat the same event id. New subscriptions watch new mail; check inbox for older mail.
A new sign-in or `baf door revoke` removes that Door agent's subscriptions and queued
events. A callback request already in flight cannot be recalled. Delivery checks the
current policy too: a removed Door agent receives no further attempts while absent.

## 8. Manage connectors and turn the door off

On the Relay:

```bash
baf door list
baf door list --json
baf door revoke openai/dot
```

These commands use the daemon's owner-only Unix socket. List shows every current
policy name, connection status, approver, approval time, and client id. Status is
`connected` when a refresh token exists, `waiting for ChatGPT` (or `waiting for the app`)
while an approved code can still be exchanged, `sign-in expired` when that code expired
unused, or `none` when there is no connector. The waiting label uses the client's
metadata URL and registered redirect to recognize ChatGPT, not the Door agent's name.
JSON output includes the same `state` and the existing `connected` boolean, with no
connector expiry.
Revoke deletes its connector, access tokens, refresh token, any unexchanged code,
subscriptions, and queued events.
It is safe to repeat and also works after the name is removed from the policy.
Neither command deletes mailbox messages.

To shut the door down:

1. Revoke its connectors while baf is running, and remove the plugin in ChatGPT.
2. Stop Funnel for this port:

   ```bash
   tailscale funnel --https=443 off
   ```

3. Set `door.enabled` to `false` in `~/.baf/config.json`, then run `baf stop` and `baf start`.
4. Remove any `/etc/hosts` line added only for this door. Restore the Mac's preferred
   sleep setting if it no longer needs to act as an always-on Relay.

## Sign-in and token details

Connectors do not expire. They stay usable until revoked, replaced by a new sign-in,
or denied by removing their name from the policy. Codes expire after five minutes
and can be exchanged once with S256 PKCE. Access tokens last one day.
Refresh tokens rotate on successful use; refreshing does not invalidate earlier,
unexpired access tokens. Existing connectors and tokens keep working: the old
connector `expires_at` database column is ignored, and new approvals store zero there.
A token is bound to exactly one Door agent and connector URL.

The Allow form has a guard bound to its browser, person, device, and original request,
expiring after ten minutes. Repeating Allow or Deny returns the first decision's exact
redirect, even if the later click chooses differently; it never issues another code.
The guard cookie expires with the form. After the code is exchanged or the form expires,
a repeat shows a finished page directing the owner back to ChatGPT. Forms and their
answers stay only in memory and do not survive a restart. Up to 1,024 forms are remembered;
expired forms lose their saved redirects and are forgotten oldest first when room is
needed. A forgotten form must be opened again. Client
metadata is fetched only from public HTTPS addresses on port 443, without redirects.
Registration is bounded to 1,000 clients; unconnected registrations older than 720 hours
are removed on the next registration request.

`door.db`, beside `mailbox.db`, stores connector details and SHA-256 hashes of random
32-byte codes and tokens. It also holds subscriptions, webhook signing secrets, and
queued event payloads; the file is owner-only. Raw OAuth codes and tokens are never
saved or logged. The owner's identity stays in the local record. Every exchange,
refresh, and bearer check consults
the current policy. Removing a name denies access immediately; revoke it too if its
old connector must remain unusable after the name is added back.

Sign-in steps appear in `~/.baf/baf.log`: `door Allow page shown`, `door sign-in allowed`
or `door sign-in denied`, then `door token issued` or `door token refused`.
Repeated form submissions log `door sign-in repeated` at info level. Allow logs
name the Door agent, approver login, and client id; token logs name the Door agent when
known and the grant type when supported. Refusals give the specific reason, including
the expected and given resource URLs for a mismatch; the client sees only
`invalid_grant`. Registration logs `door register accepted` or `door register refused`;
CIMD failures log the client metadata URL and a short reason. URLs omit queries,
fragments, and credentials. No token, code, verifier, or secret is logged.
`door bearer refused` names the Door agent from the MCP path and the reason, at most
once per minute per Door agent. Requests without a bearer token stay quiet.

The endpoint supports MCP `2026-07-28` discovery without `initialize`, and legacy
`initialize` versions `2025-11-25`, `2025-06-18`, and `2025-03-26`. It handles POST
requests without sessions or an SSE stream; `events/list` advertises `message.created`
with webhook delivery. Unknown public paths return an empty 404. Protocol mistakes
use JSON-RPC errors; mailbox refusals use tool results with `isError` and the mailbox's
plain reason.

## Door agents ↔ other machines

Upgrade baf on both the Relay and the receiving machine before trying forwarding.
The Relay sends a Dot's message to `grokbot/tokdev` with `origin: "openai/dot"`.
Grokbot asks WhoIs about the connecting Relay and checks the Dot's `door` list in
that grant. The Relay cannot read its own grant toward grokbot, so it queues the
message and lets grokbot decide. A broad `to: ["*"]` for the iMac does not expand
the Dot's permissions.

Use these grants, keeping the rest of your policy. Here `imac` and `macbook` are
existing policy aliases; use your own aliases or Tailscale addresses:

```jsonc
// Which agents on the Relay the Dot may message.
{"src": ["imac"], "dst": ["imac"], "ip": ["tcp:7780"],
 "app": {"github.com/DavidGuooo/baf/msg": [{"relay": true, "door": {"openai/dot": ["codex", "claude", "me"]}}]}},
// The Relay may forward for this Dot to tokdev on grokbot.
{"src": ["imac"], "dst": ["tag:grokbot"], "ip": ["tcp:7780"],
 "app": {"github.com/DavidGuooo/baf/msg": [{"relay": true, "door": {"openai/dot": ["tokdev"]}}]}},
// Grok bots may send to the Dot held by the Relay.
{"src": ["tag:grokbot"], "dst": ["imac", "macbook"], "ip": ["tcp:7780"],
 "app": {"github.com/DavidGuooo/baf/msg": [{"to": ["codex", "claude", "openai/dot"]}]}},
```

The existing members → `autogroup:self` and members → `tag:grokbot` grants with
`to: ["*"]` may remain. Grants add together, but only `door` lists in grants with
`relay: true` authorize a forwarded Door agent. A receiver rejects an `origin`
from a node without that permission, an unlisted Door agent, or an origin whose
vendor is a tailnet machine. The Relay vouches only for Door agents.

Each node finds the Relay by asking WhoIs about the tailnet nodes, including itself:
the one whose grants toward this node include `relay: true` is its Relay. Discovery
is cached for one minute and refreshed before retrying a failed push to the Relay.
No Relay, or more than one, gives a clear refusal when sending. No Relay address is
configured in baf. Make the Relay's grant apply toward every node that needs it.

A send to `openai/dot` on grokbot keeps that address in the message and queues it for
the Relay. The Relay accepts only Door agents named in its own grant, and only when
the pushing node's `to` list names the full address `openai/dot` or `*`. A `to` entry
of just `dot` does not allow this. Other unknown names are refused by the Relay.
The Relay's receipt means it has durably taken responsibility for the mail.

For a Dot sending to grokbot, the Relay holds the outgoing mail, retries, and rings
Bridge with `openai/dot` as the sender in both `from` fields. Rejection or the normal
24-hour handover limit puts a failure Notice in the Dot's inbox and fires its event.
The Relay rings a node only if it has a doorbell configured for that node; grokbot
needs no doorbell key for the Relay.

To try both directions after adding the grants and configuring Bridge's doorbell
on the Relay (see [Grok setup](setup-grokbot.md)):

1. Ask the Dot to watch `message.created`, then use `request` to ask
   `grokbot/tokdev` a question. Keep the receipt's id. Its `status` tool shows
   handover, then grokbot's progress; for these inbox-only bots, progress says
   whether the message was opened.
2. On grokbot, read the request and answer it:

   ```bash
   baf read --as tokdev <request-id>
   baf send openai/dot --as tokdev --kind result --reply-to <request-id> "the answer"
   baf flush --as tokdev
   ```

   If the request has a `from_thread`, include `--to-thread <from_thread>` in the
   answer. The Dot receives an event and its request's `status` names the Result.
   `baf status <result-id>` on grokbot shows handover to the Relay and whether the
   Dot opened the Result. `baf outbox --all` keeps `openai/dot` as the destination
   and shows the Relay in `VIA`.
3. On grokbot, send a request the other way:

   ```bash
   baf send openai/dot --as tokdev --kind request "a question for the Dot"
   baf flush --as tokdev
   ```

   Ask the Dot to use `reply` with that request's id. Its Result goes back through
   the Relay to `grokbot/tokdev`; `baf status <request-id>` on grokbot names it.
4. Ask the Dot to send to `grokbot/other`. With the grants above, grokbot refuses it
   and the Dot receives a failure Notice. This verifies that the iMac's broad `to`
   grant is not being used for the Dot.

A Door agent can also send to another Door agent held by this same Relay if its
local `door` list names the full destination, such as `openai/other`, or `*`.
Files attached by tailnet agents reach the Door inbox under the same permission as
the message; downloading or uploading file bytes through MCP remains unsupported.
Outbox ids remain unique on each node: reusing another agent's id is refused.
