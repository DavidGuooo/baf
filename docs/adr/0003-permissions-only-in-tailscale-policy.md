# Permissions live only in the Tailscale policy

Who may message which agent, whether they may wake it, and how much the agent may do for them (for example, Codex runs read-only) are all defined in one place: grants with an app capability in the tailnet policy file. A mailbox reads the sender's capabilities from Tailscale (`whois`) for every incoming message and enforces them. Mailboxes keep no rules file of their own. We chose this over per-mailbox rules (rules scattered across machines and drifting apart) and over a central-plus-local mix (unnecessary while every node belongs to one owner).

## Consequences

- Default deny: a sender without a matching capability is rejected, even if Tailscale lets it connect.
- Every permission change goes through the Tailscale admin console.
- One permission name (`github.com/<owner>/baf/msg`) carries every rule as fields (`to`, `relay`), so new rules are new fields, not new names.
- Talking to agents on someone else's tailnet would need this decision revisited.
