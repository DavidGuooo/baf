# Progress is asked for, not sent

A sender can follow a message it sent: handed over, its run started, what the agent is doing, done. The sender's mailbox asks the receiving mailbox when someone runs `baf status` (`GET /v1/progress/{id}` on baf's port). The receiver never sends progress messages back. We rejected progress messages because every message that reaches an agent baf runs starts a run of that agent. A progress message to Codex would start Codex again for every update, and for an inbox-only agent it would fill the inbox with mail nobody needs to keep. An answer to a question is not a message, the same as a receipt (ADR-0004). Nothing is stored, retried or answered, so no exchange can loop.

The receiver answers from its own run ledger. The agent never reports anything. Each adapter reads what its agent is doing from the output the agent prints anyway (Codex's `--json` events, Claude Code's `stream-json`) and keeps one line on the run, such as "running: go test ./...".

## Consequences

- Only the node that sent a message may ask about it, and only while its Tailscale grant still lets it message that agent (ADR-0003). Any other node is told there is no such message.
- Asking needs the receiver to be reachable. A sleeping node can't answer, but its Result still arrives when it wakes.
- Once the Result is back, the sender doesn't ask any more.
- The sender sees the commands and file names its own request led to on the receiver's machine, cut to one line.
- A node running an older baf answers 404, and the sender says that node needs updating.
- Claude Code runs with `--output-format stream-json --verbose` instead of `json`, so its tool calls can be seen as they start.
