# baf

baf (back-and-forth) is a messaging layer that lets personal agents on one person's tailnet send each other notices and requests, whoever made the agent and wherever it runs.

## Language

### Who and where

**Node**:
A machine on the tailnet, and the smallest unit whose identity Tailscale can confirm.
_Avoid_: host, peer, device

**Agent**:
Something that does work in response to messages, named uniquely on its node; it may sleep, run in a vendor's cloud, or be unable to accept connections.
_Avoid_: bot (except in product names), worker

**Address**:
`node/agent`. Tailscale vouches for the node part; the agent part is a claim made by the node. A Door agent's address is `vendor/agent` (`openai/dot`), and the Relay vouches for all of it.

**Door agent**:
An agent outside the tailnet that reaches baf only through the Public door, named in the Tailscale policy and connected by one sign-in.
_Avoid_: outside agent, remote agent

**Principal**:
Who a receiver believes it is talking to, built from Tailscale's answer and never from the message body; for a message forwarded by the Relay, the original sender the Relay recorded.
_Avoid_: sender (when it means the trusted identity)

**Reachability**:
How an agent can be contacted: **resident** (we can run a program next to it), **outbound-only** (it connects in but can't be reached), or **connector-only** (it only calls tools reachable from its vendor's cloud, so it reaches baf through the Public door).

### What moves

**Message**:
An immutable envelope whose sender-chosen id stays the same across retries.

**Notice**:
A message that expects no answer.

**Request**:
A message asking for an answer; the receiving mailbox sends back a Result automatically when the agent's run ends. Nothing on the sender's side waits for it.

**Result**:
The answer to a Request (completed, failed, rejected or unknown), naming the request it answers; nothing ever answers a Result automatically.

**Receipt**:
Proof that a Mailbox durably stored a message, and nothing more.
_Avoid_: "delivered" meaning "done"

**File**:
Bytes sent with a message. The message names each file by name, SHA-256 and size; the sending Mailbox hands the bytes over first, and the receiver stores the message only once it holds them all.

**Progress**:
What the receiving Mailbox can tell the sender about a message it stored: its run has not started, is running (and what the agent last started doing, read from the agent's own output), or is done. For an agent baf doesn't run, it says whether the message was opened. The sender's mailbox asks for it; only the sending node, or the Relay that forwarded it for a Door agent, may ask. It is never a message.
_Avoid_: progress message, status update

### Where it waits

**Mailbox**:
The durable store on an agent's own node that holds the messages the agent sends and receives; outgoing messages wait there until handed over.
_Avoid_: inbox, outbox (they are views of one mailbox), server, hub

**Handover**:
Moving a message from the sender's mailbox to the receiver's: by **push** when the receiver's node is reachable, or by **pickup** when the receiver can only connect outward and fetches it.
_Avoid_: delivery (when it means "done")

**Wake**:
A hint that an agent should check its mailbox now; the receiving side decides, and a wake carries no authority.

**Doorbell**:
A way to wake an agent that works even while its node is offline (the routine webhook of Grok's Bridge bot; MCP events for a Dot); the agent hands its key only to peers it trusts.

**Relay**:
The one mailbox per network, named once in the Tailscale policy, that holds messages where peer-to-peer can't work (neither side can accept connections, or one side is outside the tailnet) and vouches for the original sender.
_Avoid_: hub, server

**Public door**:
The Relay's way in for Door agents: a sign-in page reachable only from the tailnet, and the `baf` command's actions over Funnel with a token that names one Door agent.
_Avoid_: gateway, bridge, tunnel

### Continuity

**Thread**:
An agent's own conversation state, owned by that agent and meaningless to anyone else.
_Avoid_: session_id

**Thread id**:
The id of one side's Thread. Every message carries the sender's (`from_thread`) and, to continue a thread, the receiver's (`to_thread`).
_Avoid_: conversation id (replaced)

**Run**:
One attempt by an agent to carry out a Request; a run whose outcome is unknown is never repeated automatically, and the sender decides whether to try again.

**Run folder**:
The folder on the receiving node where a thread's work happens, chosen when the thread starts and kept for its follow-ups.
_Avoid_: sender's folder, agent per repository

**Desktop hand-off**:
Giving a Claude thread to the owner in the Claude Desktop app after its run ends. The app then owns continuation of that thread; the owner can have Claude report the outcome with a Notice.
_Avoid_: automatic Result, shared session

**Agent adapter**:
What connects a Mailbox to one kind of agent: how to run or wake it, and how its threads and outcomes map back.
