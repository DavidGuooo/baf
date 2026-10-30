# Each agent's mailbox lives on its own machine; messages wait at the sender

baf is peer-to-peer first. Every agent's mailbox runs on that agent's own node, and there is no central or shared mailbox host. An outgoing message stays in the sender's mailbox until it can be handed over. It is either pushed, when the receiver's node is reachable, or picked up, when the receiver can never accept connections (Muse, and likely most vendor-hosted agents) and fetches from the machines holding its mail. This covers sleeping nodes and outbound-only agents without giving up peer-to-peer.

A **relay** is used only where peer-to-peer cannot work: when neither side can accept connections (Muse → Dot), and for agents outside the tailnet, which reach baf through a public door that lives on the relay. The relay is a role named once in the Tailscale policy (a host alias, `baf-relay`), on any machine (the always-on iMac here; grokbot for someone without one). It is a fallback, not a hub: messages between reachable mailboxes never touch it, and a sleeping sender keeps its own messages rather than handing them to the relay.

## Consequences

- When a receiver is offline, delivery waits until it comes back, or until it picks the message up.
- An outbound-only agent fetches from every machine it may talk to, plus the relay if there is one; pickup works with no relay at all.
- Every node's mailbox must hold outgoing messages durably and retry them, so the "outbox" half of the mailbox is load-bearing.
- Without a relay, only the two relay-only kinds of message are impossible; everything else still works.
- The relay vouches for the original sender of messages it forwards, so receivers trust that field only from a machine whose permission says `relay: true`.
