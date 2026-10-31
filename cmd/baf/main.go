// Command baf is the mailbox daemon and the command agents use to send and
// read messages.
//
//	baf serve                         run the mailbox (keep it running)
//	baf start | stop                  run the mailbox in the background, or stop it
//	baf send ADDR [TEXT]              send a notice (no answer expected)
//	baf request ADDR [TEXT]           send a request (a Result comes back)
//	baf inbox [--all]                 list unread messages
//	baf read ID                       show one message and mark it read
//	baf status ID                     follow a message you sent: handed over, running, answered
//	baf outbox [--all]                list messages waiting to go to other machines
//	baf runs                          list recent agent runs
//	baf wait ID...                    for routine runs: wait for mail to arrive
//	baf flush                         for bot runs: wait for your sent mail to leave
//
// ADDR is node/agent, or just agent for an agent on this machine. send and
// request take --file PATH (repeatable) to attach files; TEXT may then be left out.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/DavidGuooo/baf/internal/config"
	"github.com/DavidGuooo/baf/internal/daemon"
	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/DavidGuooo/baf/internal/mailbox"
	"github.com/DavidGuooo/baf/internal/tailnet"
)

const usage = `baf: messages between your agents over Tailscale

Usage:
  baf serve                      run the mailbox daemon in the foreground
  baf start | stop               run it in the background (log: ~/.baf/baf.log), or stop it
  baf send ADDR [TEXT...]        send a notice (expects no answer)
  baf request ADDR [TEXT...]     send a request (a Result comes back automatically)
  baf inbox [--all]              list unread messages on this machine (--all: read ones too)
  baf read ID                    show one message and mark it read
  baf status ID                  follow a message you sent: handed over, running (and doing what), answered
  baf outbox [--all]             messages waiting to go to other machines (--all: sent ones too)
  baf runs [--limit N]           list recent agent runs
  baf wait [--as NAME] ID...     wait until these messages have arrived (default --timeout 10m)
  baf flush [--as NAME]          wait until NAME's sent messages have left this machine (default --timeout 10m)

ADDR is node/agent, or just agent for one on this machine. TEXT "-" reads stdin;
it may be left out when you attach files.

Options for send and request:
  --file PATH        attach a file (repeatable; up to 20, 100 MB each)
  --as NAME          send as this agent (default $BAF_AGENT, else "me")
  --to-thread ID     continue the receiver's thread with this id
  --from-thread ID   your own thread id (default $BAF_THREAD)
  --kind KIND        notice, request or result (send only)
  --reply-to ID      the request a result answers
  --state STATE      a result's outcome: completed (default), failed, rejected, unknown

Options for inbox, read and wait:
  --as NAME          only messages addressed to NAME (default $BAF_AGENT; none = every agent's)
  --json             print JSON (inbox, read, status, outbox and runs)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "start":
		err = start(args)
	case "stop":
		err = stop(args)
	case "wait":
		err = wait(args)
	case "flush":
		err = flush(args)
	case "send":
		err = send(args, envelope.Notice)
	case "request":
		err = send(args, envelope.Request)
	case "inbox":
		err = inbox(args)
	case "read":
		err = read(args)
	case "status":
		err = status(args)
	case "outbox":
		err = outbox(args)
	case "runs":
		err = runs(args)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "baf: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "baf:", err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	if _, err := parse(fs, args); err != nil {
		return err
	}
	home, err := config.Home()
	if err != nil {
		return err
	}
	cfg, err := config.Load(home)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return daemon.Serve(ctx, home, cfg, &tailnet.Client{})
}

func send(args []string, kind envelope.Kind) error {
	fs := flag.NewFlagSet(string(kind), flag.ExitOnError)
	as := fs.String("as", defaultAgent(), "")
	toThread := fs.String("to-thread", "", "")
	fromThread := fs.String("from-thread", os.Getenv("BAF_THREAD"), "")
	kindFlag := fs.String("kind", string(kind), "")
	replyTo := fs.String("reply-to", "", "")
	state := fs.String("state", "", "")
	var paths filePaths
	fs.Var(&paths, "file", "attach a file (repeatable; up to 20, 100 MB each)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 || (len(pos) < 2 && len(paths) == 0) {
		return errors.New("usage: baf " + string(kind) + " ADDR [TEXT...] [--file PATH]...")
	}
	files, err := openFiles(paths)
	if err != nil {
		return err
	}
	defer closeFiles(files)
	text := strings.Join(pos[1:], " ")
	if text == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		text = string(b)
	}

	c, err := client()
	if err != nil {
		return err
	}
	to := pos[0]
	if !strings.Contains(to, "/") {
		st, err := c.Status()
		if err != nil {
			return err
		}
		to = st.Node + "/" + to
	}
	env := envelope.Envelope{
		V:          envelope.Version,
		ID:         envelope.NewID(),
		Kind:       envelope.Kind(*kindFlag),
		To:         to,
		FromAgent:  *as,
		FromThread: *fromThread,
		ToThread:   *toThread,
		ReplyTo:    *replyTo,
		Body:       envelope.Body{Text: text, State: resultState(envelope.Kind(*kindFlag), *state)},
	}
	for i, f := range files {
		file, err := c.AddFile(filepath.Base(paths[i]), f)
		if err != nil {
			return fmt.Errorf("file %q: %w", paths[i], err)
		}
		env.Body.Files = append(env.Body.Files, file)
	}
	r, err := c.Send(env)
	if err != nil {
		return err
	}
	note := ""
	if len(files) > 0 {
		note = " (" + fileCount(len(files)) + ")"
	}
	if r.Duplicate {
		note += " (sent earlier with the same id)"
	}
	if r.Queued {
		fmt.Printf("%s %s queued for %s%s; it waits here until that machine stores it\n",
			env.Kind, r.ID, to, note)
	} else {
		fmt.Printf("%s %s stored in %s's mailbox%s\n", env.Kind, r.ID, to, note)
	}
	fmt.Printf("follow it: baf status %s\n", r.ID)
	return nil
}

// resultState is the state a sent message carries. A result written by hand
// is "completed" unless it says otherwise: its text explains the rest.
func resultState(kind envelope.Kind, state string) envelope.ResultState {
	if kind == envelope.Result && state == "" {
		return envelope.Completed
	}
	return envelope.ResultState(state)
}

func inbox(args []string) error {
	fs := flag.NewFlagSet("inbox", flag.ExitOnError)
	as := fs.String("as", os.Getenv("BAF_AGENT"), "") // no name = every agent's messages
	all := fs.Bool("all", false, "")
	asJSON := fs.Bool("json", false, "")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	c, err := client()
	if err != nil {
		return err
	}
	items, err := c.Inbox(*as, *all)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(items)
	}
	if len(items) == 0 {
		if *as == "" {
			fmt.Println("no messages")
		} else {
			fmt.Printf("no messages for %s\n", *as)
		}
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTO\tKIND\tFROM\tRECEIVED\tTEXT")
	for _, it := range items {
		kind := string(it.Message.Kind)
		if it.Message.Body.State != "" {
			kind += " (" + string(it.Message.Body.State) + ")"
		}
		_, to, _ := strings.Cut(it.Message.To, "/")
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", it.Message.ID, to, kind, it.From,
			it.ReceivedAt.Local().Format("Jan 2 15:04"), inboxText(it))
	}
	return tw.Flush()
}

func read(args []string) error {
	fs := flag.NewFlagSet("read", flag.ExitOnError)
	// Unlike other commands there is no "me" default: with no name given,
	// the id alone finds the message, whoever it is for.
	as := fs.String("as", os.Getenv("BAF_AGENT"), "")
	asJSON := fs.Bool("json", false, "")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: baf read ID")
	}
	c, err := client()
	if err != nil {
		return err
	}
	it, err := c.Read(*as, pos[0])
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(it)
	}
	printItem(os.Stdout, it)
	return nil
}

func printItem(w io.Writer, it mailbox.Item) {
	m := it.Message
	fmt.Fprintf(w, "id:          %s\nkind:        %s\nfrom:        %s\n", m.ID, m.Kind, it.From)
	if m.Kind == envelope.Result {
		fmt.Fprintf(w, "answers:     %s\nstate:       %s\n", m.ReplyTo, m.Body.State)
	}
	if m.FromThread != "" {
		fmt.Fprintf(w, "from_thread: %s   (reply with --to-thread %s)\n", m.FromThread, m.FromThread)
	}
	if m.ToThread != "" {
		fmt.Fprintf(w, "to_thread:   %s\n", m.ToThread)
	}
	fmt.Fprintf(w, "received:    %s\n", it.ReceivedAt.Local().Format(time.DateTime))
	for _, f := range it.Files {
		fmt.Fprintf(w, "file:        %s (%s)  %s\n", f.Name, humanSize(f.Size), f.Path)
	}
	fmt.Fprintf(w, "\n%s\n", m.Body.Text)
}

func outbox(args []string) error {
	fs := flag.NewFlagSet("outbox", flag.ExitOnError)
	all := fs.Bool("all", false, "")
	asJSON := fs.Bool("json", false, "")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	c, err := client()
	if err != nil {
		return err
	}
	items, err := c.Outbox(*all)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(items)
	}
	if len(items) == 0 {
		fmt.Println("nothing waiting to be sent")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tKIND\tFROM\tTO\tSTATE\tTRIES\tNEXT TRY\tGIVES UP\tLAST ERROR")
	for _, it := range items {
		next, until := "-", "-"
		if it.State == "waiting" {
			next = it.NextTry.Local().Format("15:04:05")
			until = it.ExpiresAt.Local().Format("Jan 2 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", it.ID, it.Kind, it.FromAgent, it.To, it.State,
			it.Attempts, next, until, firstLine(it.LastError, 60))
	}
	return tw.Flush()
}

func runs(args []string) error {
	fs := flag.NewFlagSet("runs", flag.ExitOnError)
	limit := fs.Int("limit", 20, "")
	asJSON := fs.Bool("json", false, "")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	c, err := client()
	if err != nil {
		return err
	}
	list, err := c.Runs(*limit)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(list)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tAGENT\tSTATE\tTHREAD\tMESSAGE\tFROM\tSTARTED\tTOOK\tDOING (LAST)")
	for _, r := range list {
		took := "-"
		if r.EndedAt != nil {
			took = r.EndedAt.Sub(r.StartedAt).Round(time.Second).String()
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s %s\t%s\t%s\t%s\t%s\n", r.ID, r.Agent, r.State, r.ThreadID,
			r.Kind, r.MessageID, r.From, r.StartedAt.Local().Format("Jan 2 15:04:05"), took, firstLine(r.Doing, 50))
	}
	return tw.Flush()
}

func client() (*daemon.Client, error) {
	home, err := config.Home()
	if err != nil {
		return nil, err
	}
	return daemon.NewClient(daemon.SocketPath(home)), nil
}

func defaultAgent() string {
	if a := os.Getenv("BAF_AGENT"); a != "" {
		return a
	}
	return "me"
}

// parse reads flags that may appear before, between or after the positional
// arguments, which the standard flag package alone does not allow.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		if args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func firstLine(s string, n int) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
