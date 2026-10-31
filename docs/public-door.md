# Public door sign-in

The Relay holds each Door agent named in its Tailscale grant. Sign-in connects one
OAuth client to one Door agent; what it may message still comes only from the policy
([ADR-0008](adr/0008-outside-agents-enter-by-the-door-under-the-policy.md)).

Sign-in and tokens are implemented. MCP tools and events are not: an authenticated
request to the connector URL currently returns **501**, so it is not yet a usable
messaging connector. Forwarding to other machines remains unsupported.

## Configuration

On the Relay, add this to baf's config:

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

The door is disabled by default. `public_url` is required when enabled and must name
this Relay's Tailscale DNS name, using HTTPS on port 443 with no path. The other two
settings shown are their defaults. `public_listen` must be a loopback IP and port;
`signin_port` must differ from 443 and baf's mailbox port (normally 7780). Invalid
settings stop startup with an error.

The Relay's own capability grant must name the Door agents, for example:

```json
{"relay": true, "door": {"openai/dot": ["codex", "claude", "me"]}}
```

For `openai/dot`, the connector URL is:

```text
https://imac.example.ts.net/door/openai/dot/mcp
```

The public listener serves only those connector paths, their protected-resource
metadata at `/.well-known/oauth-protected-resource/door/<vendor>/<name>/mcp`,
`/.well-known/oauth-authorization-server`, `/register`, and `/token`. Other paths
return an empty 404. Requests and headers are limited to 16 KiB.

## Network setup

HTTPS certificates and Funnel must already be enabled for the Relay in Tailscale.
Arrange Funnel's HTTPS port 443 to forward to `http://127.0.0.1:7790` (or the
configured `public_listen`). baf does not configure Funnel or Tailscale Serve.

`baf serve` starts its own HTTPS sign-in listener on **each of the Relay's Tailscale
addresses**, at `signin_port`, using certificates from Tailscale's local API. The
policy must allow the owner's devices to reach that port. The Allow page is
`https://imac.example.ts.net:8444/authorize`; the OAuth metadata directs the
browser there with the required query parameters. It is not published through
Funnel. A Mac cannot reach its own Tailscale Serve port, which is why baf runs this
listener itself.

A proxy in TUN mode can prevent Tailscale from obtaining a certificate if its DNS
requests go to the proxy's fake DNS address. It can also prevent the owner's browser
from resolving the Relay's `ts.net` name. Check certificate availability and name
resolution separately; the probe worked after obtaining the certificate with the
proxy off and resolving the Relay's name to its Tailscale address locally.

## Approval and lifetime

The Allow page identifies the person using WhoIs on the connection's address.
Tagged devices cannot approve. A sign-in also requires a Door agent in the current
policy, a registered redirect, and valid S256 PKCE. The form has a one-use guard
bound to the browser, person, device, and original authorization request; it expires
after ten minutes. Pending forms do not survive a daemon restart.

ChatGPT's HTTPS client metadata document (CIMD) is supported, as is dynamic client
registration. Redirects must use HTTPS. Client metadata is fetched only over HTTPS
on port 443 from public addresses, with no redirects. Registration stores at most
1,000 clients; unconnected registrations older than 30 days are removed on the next
registration request.

For `openai` names, the page warns when a URL client id or redirect is outside
`chatgpt.com` / `openai.com` and their subdomains. Opaque registered client ids are
checked using the redirect. Other vendors also get a warning until their matching
rules are defined. These are warnings, not additional policy permissions.

The owner selects 7, 30, 90, or 365 days (default 30). Approval immediately replaces
any earlier connector for that Door agent. Codes expire after five minutes and can
be exchanged once. Access tokens last one day, capped by the connector's expiry;
refresh tokens rotate on each successful use until that expiry. Refreshing does not
extend the connector's lifetime or invalidate earlier, unexpired access tokens.
The token request may omit `resource` to use the approved resource; an explicitly
different resource is refused.

`door.db`, beside `mailbox.db`, stores connector details and SHA-256 hashes of random
32-byte codes and tokens. Raw codes and tokens are never saved or logged. The owner
identity stays in the local connector record; only OAuth fields, including `iss`,
return to the client. Every code exchange, refresh, and bearer check consults the
current policy. Removing a name denies access immediately; revoke it too if its old
connector must remain unusable after the name is added back.

## Local management

```bash
baf door list
baf door list --json
baf door revoke openai/dot
```

These use the daemon's owner-only Unix socket. List shows all current policy names,
connector status, approver, approval time, client id, and expiry. A pending approval
or expired connector retains its local details but is not shown as connected.
Revoke deletes the connector, all its access tokens, its refresh token, and any
unexchanged code. It also works after a name has been removed from the policy and
is safe to repeat. Neither command deletes mailbox messages.
