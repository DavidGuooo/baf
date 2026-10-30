# Build our own messaging layer; Yobiru is a reference only

Yobiru (tailmsg v1) is already installed on imac and moves Grok's messages successfully. We are still building baf from scratch and not on top of it: no shared code, no wire compatibility, no dependency on `~/.tailmsg`. The design needs things that Yobiru's protocol and model don't allow:
- envelope fields Yobiru's receiver rejects as unknown (`from_thread`, `to_thread`, `deadline`);
- agent runs started by the mailbox itself (headless `codex exec` / `claude -p`), so the mailbox always knows whether a run started;
- authorization carried in tailnet grants instead of per-machine config;
- a relay that holds mail for agents that can only make outbound connections, and vouches for the original sender.

We still read Yobiru's source for its hard-won failure handling (dedupe, durable ledger, receipts kept separate from results, file pulls checked by hash).

## Consequences

- During development both run side by side on imac. baf must not use Yobiru's port `7766` and must not read or write `~/.tailmsg`.
- Grok Bot will need baf's own client on its computer, or a routine that speaks baf. It can't rely on its Yobiru install.
