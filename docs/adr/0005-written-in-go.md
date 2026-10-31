# baf is written in Go

baf (the mailbox daemon, the relay role and the `baf` command agents call) is written in Go. Two things decided it. First, people should install Tailscale plus baf and nothing else: Go builds one self-contained file per platform, with no runtime to install on macOS or on Linux machines like grokbot. Second, baf asks Tailscale who is connecting, and with what permission, for every message: Tailscale is written in Go and ships an official Go client for that question, which also handles the macOS app, where reaching Tailscale is harder than on Linux. tsnet, which would give each agent its own Tailscale identity later, exists only in Go. We considered TypeScript (the owner's usual language, and the most mature MCP library for the public door), but it needs Node or a bundling step and must call the `tailscale` command for every message. We also considered Python (Yobiru's language), but it relies on whatever Python version is installed, and the door would add packages to install.

## Consequences

- Releases are one binary per platform (macOS arm64/amd64, Linux amd64/arm64).
- Identity and permissions come from Tailscale's Go client, not from calling the `tailscale` command.
- The Public door uses standard-library HTTP and JSON, porting the MCP discovery and tool shapes accepted in the door probe. This replaces the earlier plan to use the official Go MCP SDK; the small current surface needs no new dependency. Events are a later job.
- The owner hasn't used Go in other projects. The code should stay plain and well commented.
