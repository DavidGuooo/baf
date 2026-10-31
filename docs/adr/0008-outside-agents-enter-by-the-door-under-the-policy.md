# Outside agents enter by the door, under the policy

Agents that can't join the tailnet (a ChatGPT Dot) reach baf through the Public door on the Relay, as Door agents with addresses of their own, such as `openai/dot`. Each sign-in on the door's Allow page connects one vendor connector to one Door agent, and it says only *who* the connector is and for how long. *What* a Door agent may message, and who may message it, stays in the Tailscale policy like every other permission (ADR-0003). We considered keeping that list on the sign-in, ticked on the Allow page: quicker to change, but it would have put rules outside the policy, on a machine agents can write to. We also considered addressing Door agents as agents on the Relay's own node (`imac/dot`), which needs no routing or vouching, but then every receiver would see them as the Relay's machine and could not tell them apart in the policy.

## Considered Options

- **Door on Grok's machine.** Tested on 2026-10-31. Funnel works there once Tailscale's operator is set, but grokbot sleeps a few minutes after its last use, and after a wake its public side came back only after 5½ minutes, when it reconnected to Tailscale's coordination server. A Door agent can't wait that long, so the Relay that hosts the door must be a machine that doesn't sleep.
- **OpenAI's Secure MCP Tunnel instead of Funnel.** Not tried: ChatGPT only, and its sign-in server still has to be public. Anthropic's equivalent is for Enterprise organizations only.

## Consequences

- The Relay's grant in the policy names each Door agent and what it may message (a `door` field next to `relay: true`). Receivers enforce it: a message the Relay forwards from a Door agent is checked against that list, not against the Relay's own permissions.
- The Relay vouches only for Door agents. A forwarded message claiming a sender on a tailnet machine is refused.
- Other machines send mail for a name that isn't a tailnet machine to the Relay; the Relay refuses names it doesn't hold.
- A Door agent exists only once the policy names it. The Allow page refuses a sign-in for a name the policy doesn't list, and signing in again for the same name replaces the earlier connector.
- Changing what a Dot may message is an admin-console edit, never a new sign-in.
