package main

import (
	"fmt"
	"os"
	"strings"
)

// guideText is for an agent that runs baf for agents that can't (Bridge on
// grokbot, for Grok's bots). It ships with baf, so it changes with baf and
// the agent's own instructions only point here. {baf} becomes this binary's
// path, so every command works as printed.
const guideText = `baf guide (baf {version}): carrying baf messages for other agents

You run baf for agents that can't run it themselves. Act as the agent you carry for:
--as <its name, lowercase>. Pass message text on stdin exactly as the agent wrote it:
end the command with - <<'BAF', then the text, then BAF on its own line.

SENDING FOR AN AGENT
1. New message: {baf} send <address> --as <agent> --from-thread <agent> --to-thread <thread> -
   If it needs an answer, use request instead of send.
2. Reply to baf message <id>: {baf} send <from> --as <agent> --kind result --reply-to <id> --from-thread <agent> --to-thread <thread> -
3. <thread> keeps the conversation together. For a reply, it is the from_thread of message
   <id>: {baf} read --as <agent> <id> shows it and <from>. For a new message, it is the
   from_thread of the latest message that address sent the agent: find it with
   {baf} inbox --as <agent>. If there is none, leave out --to-thread.
4. Files: add --file <path> for each file the agent gives you.
5. If the agent names a folder on the receiving machine, add --dir <path relative to that
   machine's home>. If it asks for the Claude Desktop app, use send (a notice, never
   request) with --desktop. Leave out --to-thread for either option: they start a new thread.
6. After sending, run {baf} flush, then tell the agent whether it was sent: the message id
   (it needs it to ask for the status later), or the error.
7. Status: {baf} outbox --as <agent> lists what the agent sent, and {baf} status <id>
   follows one message.

WHEN BAF RINGS
A doorbell is a webhook whose JSON body names the new messages, for example:
{"baf":1,"from":"imac","messages":[{"id":"01JC...","from":"imac/codex","to":"grokbot/tokdev","kind":"request"}]}
1. Run {baf} start.
2. Run {baf} wait --as <agent> <id>. The id comes from the body, and the agent is the name
   after the "/" in "to". This waits until the message has arrived.
3. Run {baf} read --as <agent> <id>.
4. If no agent has that name, run this, with <agents> replaced by the names of the agents
   that exist, lowercase and comma-separated, then run {baf} flush and go to step 6:
   {baf} send <from> --as <agent> --kind result --reply-to <id> --state rejected "There is no agent named <agent> here. Available: <agents>"
5. Give the agent the message, filled in from what baf read showed. If it is addressed to
   you, handle it yourself.
   "Baf message <id> from <from> (<kind>; for a result, also: answering <the id it answers>):
   <the message text, exactly>
   <any files it lists, with their paths>
   <for a request:> Send me your reply and I'll send it back."
6. Run {baf} inbox and handle each message not yet read (READ is no) with steps 3-5. This
   catches what no ring announces, such as the notice baf sends when a request got no
   answer within an hour.
7. Stop.

When you only forward a message to an agent or send one for it, write nothing in your own
chat. A sender may be a Door agent such as openai/dot; answer it like any other address.
`

func guide() {
	bin, err := os.Executable()
	if err != nil {
		bin = "baf"
	}
	if strings.ContainsAny(bin, " '\"$`\\") {
		bin = "'" + strings.ReplaceAll(bin, "'", `'\''`) + "'"
	}
	text := strings.ReplaceAll(guideText, "{baf}", bin)
	fmt.Print(strings.ReplaceAll(text, "{version}", versionString()))
}
