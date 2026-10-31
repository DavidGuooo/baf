# Setting up the Public door: ChatGPT Dot ↔ agents on the Relay

The Public door lets a ChatGPT Dot use baf as `openai/dot`. The Relay holds its mail,
checks its permissions in the Tailscale policy, and exposes six MCP tools through
Funnel. Only agents on the Relay can exchange mail with it in this version. Events
and file transfers through the door are not implemented yet; the Dot must check `inbox`.

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
share the node's trust. Messages from other machines are still refused in this
version, even if their grants name `openai/dot`. Cross-machine routing is a later job.
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
   Tagged devices cannot approve. Choose a lifetime and select **Allow**.
5. Check that the connector lists `send`, `request`, `reply`, `inbox`, `read`, and `status`.
   There are no events yet. A second Door agent needs its own policy entry and connector URL.

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
in this example. A destination on another machine gets a clear refusal.

Ask the Dot to **request** an answer from `codex`. It gets a receipt immediately; when
Codex's run finishes, the Result appears in the Dot's **inbox**. **status** reports
progress and the Result id. The Dot must check inbox until events are added.

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

## 7. Manage connectors and turn the door off

On the Relay:

```bash
baf door list
baf door list --json
baf door revoke openai/dot
```

These commands use the daemon's owner-only Unix socket. List shows every current
policy name, connection status, approver, approval time, client id, and expiry.
Revoke deletes its connector, access tokens, refresh token, and any unexchanged code.
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

The owner chooses 7, 30, 90, or 365 days (default 30). Codes expire after five minutes
and can be exchanged once with S256 PKCE. Access tokens last one day, capped by the
connector's expiry. Refresh tokens rotate on successful use; refreshing does not
extend the connector's lifetime or invalidate earlier, unexpired access tokens.
A token is bound to exactly one Door agent and connector URL.

The Allow form has a one-use guard bound to its browser, person, device, and original
request, expiring after ten minutes. Pending forms do not survive a restart. Client
metadata is fetched only from public HTTPS addresses on port 443, without redirects.
Registration is bounded to 1,000 clients; unconnected registrations older than 30 days
are removed on the next registration request.

`door.db`, beside `mailbox.db`, stores connector details and SHA-256 hashes of random
32-byte codes and tokens. Raw codes and tokens are never saved or logged. The owner's
identity stays in the local record. Every exchange, refresh, and bearer check consults
the current policy. Removing a name denies access immediately; revoke it too if its
old connector must remain unusable after the name is added back.

The endpoint supports MCP `2026-07-28` discovery without `initialize`, and legacy
`initialize` versions `2025-11-25`, `2025-06-18`, and `2025-03-26`. It handles POST
requests without sessions or an SSE stream; `events/list` currently returns an empty
list. Unknown public paths return an empty 404. Protocol mistakes use JSON-RPC errors;
mailbox refusals use tool results with `isError` and the mailbox's plain reason.
