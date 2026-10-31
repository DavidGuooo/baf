# Files go ahead of the message, named by their hash

A message can carry files. The envelope holds only each file's name, SHA-256 and size (`body.files`); the bytes travel separately. The sending mailbox hands each file's bytes to the receiving mailbox first (`PUT /v1/files/{sha256}?to=AGENT` on baf's port), then pushes the message. The receiver stores the message only once it holds every file the message names, so a receipt still means everything is on the receiver's disk (ADR-0004). We rejected putting the bytes inside the envelope: every retry would resend them, and the push and the database would grow with them. We also rejected having the receiver pull the files from the sender later, as Yobiru does. The sender may be asleep by then (grokbot), and the receiver would need its own retry queue and its own way to say "done".

The bytes are named by their hash, so the receiver stores a file once however often it is sent, and a file that arrives damaged never matches. A retry sends the bytes again; skipping files the receiver already holds can come later if bandwidth matters.

## Consequences

- A node accepts a file only from a node that may message the agent named in `to` (ADR-0003). It checks the size and hash while writing, and refuses files over 100 MB and messages with more than 20 files.
- `baf send` and `baf request` take `--file PATH`, repeatable. The command streams the bytes to its own mailbox, so the mailbox never opens a path an agent chose, and the message carries the file as it was when sent.
- On the receiving machine, each file is saved as `files/in/<sender node>/<message id>/<name>` under baf's home. The agent's prompt lists those paths, and `baf read` shows them. They are kept as long as the message is.
- File names are plain names: no `/`, `\` or control characters, at most 255 bytes, and no two the same ignoring case.
- Automatic Results carry no files. An agent that wants to send a file back sends its own message with `--file`.
- A node running an older baf answers the file upload with a plain 404. The sender then fails the message and says that node needs updating, rather than sending a message whose files would be lost.
- Bytes waiting to be handed over are kept until the message is handed over or given up. Bytes no message needs any more are removed after a day.
