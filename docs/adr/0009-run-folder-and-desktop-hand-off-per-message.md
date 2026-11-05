# Choose the run folder and Desktop hand-off per message

The sender may choose a run folder and a Claude Desktop hand-off on a message that starts a new thread. This keeps one rule set instead of configuring an agent per repository. Folder permission is a `dirs` field of the existing Tailscale grant (ADR-0003); the receiver resolves the folder before accepting the message and records it for every follow-up.

A Desktop hand-off is only for a Notice starting a new thread: a Request's automatic Result would report a first pass while the owner is still working. The CLI and the app writing one session forks its transcript, and the app does not show turns the CLI adds. The thread-share check was dropped in 44045c4; instead, a handed-off thread refuses `to_thread`, keeping the app its only writer.

## Consequences

- Claude runs without an explicit folder start in a new scratch folder per thread, using the Desktop app's layout on macOS when its account is available.
- A Desktop run speaks as `me`, carries no baf agent or thread identity in its environment, and tells the owner how to send the outcome as a Notice. Opening the app is best effort after recording the run.
- Door agents cannot choose run options.
