package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/DavidGuooo/baf/internal/mailbox"
)

// status follows a message one of this machine's agents sent: whether it
// was handed over, what the receiver says it is doing, and whether a Result
// came back.
func status(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: baf status ID")
	}
	c, err := client()
	if err != nil {
		return err
	}
	s, err := c.SentStatus(pos[0])
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(s)
	}
	printStatus(os.Stdout, s, time.Now())
	return nil
}

// printStatus writes one line per thing observed, in the order it happens.
func printStatus(w io.Writer, s mailbox.SentStatus, now time.Time) {
	line := func(label, format string, args ...any) {
		fmt.Fprintf(w, "  %-12s %s\n", label, fmt.Sprintf(format, args...))
	}
	sender := s.FromAgent
	if s.Origin != "" {
		sender = s.Origin
	}
	fmt.Fprintf(w, "%s %s from %s to %s\n", s.Kind, s.ID, sender, s.To)
	line("sent", "%s", stamp(s.SentAt))

	if h := s.Handover; h != nil {
		node, _, _ := strings.Cut(s.To, "/")
		if h.Via != "" && h.Via != node {
			line("via Relay", "%s", h.Via)
		}
		switch h.State {
		case "handed":
			line("handed over", "%s (%s)", stamp(deref(h.DoneAt)), tries(h.Attempts))
		case "failed":
			line("not sent", "%s", h.LastError)
		default:
			line("waiting", "%s so far; next try %s, gives up %s", tries(h.Attempts),
				h.NextTry.Local().Format("15:04:05"), stamp(h.ExpiresAt))
			if h.LastError != "" {
				line("last error", "%s", firstLine(h.LastError, 100))
			}
		}
	}

	node, _, _ := strings.Cut(s.To, "/")
	if s.Handover != nil && s.Handover.Via != "" {
		node = s.Handover.Via
	}
	switch p := s.Receiver; {
	case s.ReceiverError != "":
		line("receiver", "could not ask %s: %s", node, s.ReceiverError)
	case p == nil:
	case p.State == "inbox":
		_, agentName, _ := strings.Cut(p.To, "/")
		told := ""
		if p.NotifiedAt != nil {
			told = "; told " + stamp(*p.NotifiedAt)
		}
		if p.ReadAt != nil {
			line("receiver", "in %s's inbox%s; opened %s", agentName, told, stamp(*p.ReadAt))
		} else {
			line("receiver", "in %s's inbox%s; not opened yet", agentName, told)
		}
	case p.Run == nil:
		line("receiver", "stored %s; its run has not started (another run may be using its thread)", stamp(p.StoredAt))
	case p.Run.EndedAt == nil:
		line("run", "running for %s", since(p.Run.StartedAt, now))
	default:
		line("run", "%s after %s", p.Run.State, p.Run.EndedAt.Sub(p.Run.StartedAt).Round(time.Second))
	}
	if p := s.Receiver; p != nil && p.Run != nil && p.Run.Doing != "" {
		label, when := "last action", ""
		if p.Run.EndedAt == nil {
			label = "doing"
		}
		if p.Run.DoingAt != nil {
			when = fmt.Sprintf(" (started %s ago)", since(*p.Run.DoingAt, now))
		}
		line(label, "%s%s", p.Run.Doing, when)
	}

	switch {
	case s.Result != nil:
		line("result", "%s, %s (read it: baf read %s)", s.Result.State, stamp(s.Result.ReceivedAt), s.Result.ID)
		if s.Result.ReceivedAt.After(s.SentAt.Add(mailbox.AnswerTimeout)) {
			line("answer", "came after the 1h deadline")
		}
	case s.AnswerExpiredAt != nil:
		line("result", "no answer within 1h (told the sender %s)", s.AnswerExpiredAt.Local().Format("15:04:05"))
	case s.Kind == "request":
		line("result", "not yet")
	}
}

func stamp(t time.Time) string { return t.Local().Format("Jan 2 15:04:05") }

func since(t, now time.Time) time.Duration { return max(now.Sub(t), 0).Round(time.Second) }

func tries(n int) string {
	if n == 1 {
		return "1 try"
	}
	return fmt.Sprintf("%d tries", n)
}

func deref(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
