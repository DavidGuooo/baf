package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/DavidGuooo/baf/internal/config"
	"github.com/DavidGuooo/baf/internal/daemon"
)

// These commands are for machines where nothing keeps baf running, like
// grokbot: a routine run brings the mailbox up and waits for the messages its
// doorbell announced.

// start runs `baf serve` in the background unless it is already running,
// and waits until it answers.
func start(args []string) error {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	timeout := fs.Duration("timeout", 90*time.Second, "")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	c, err := client()
	if err != nil {
		return err
	}
	if st, err := c.Status(); err == nil {
		fmt.Printf("baf is already running on %s\n", st.Node)
		return nil
	}
	home, err := config.Home()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logPath := filepath.Join(home, "baf.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(exe, "serve")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // outlives this command and its terminal
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	os.WriteFile(pidPath(home), []byte(strconv.Itoa(pid)), 0o600)
	cmd.Process.Release()

	deadline := time.Now().Add(*timeout)
	for time.Now().Before(deadline) {
		if st, err := c.Status(); err == nil {
			fmt.Printf("baf started on %s (pid %d, log %s)\n", st.Node, pid, logPath)
			return nil
		}
		if syscall.Kill(pid, 0) != nil {
			return fmt.Errorf("baf exited during startup; see %s", logPath)
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("baf did not answer within %s; see %s", *timeout, logPath)
}

// stop asks a daemon started by `baf start` to shut down cleanly, and waits.
func stop(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	if _, err := parse(fs, args); err != nil {
		return err
	}
	home, err := config.Home()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(pidPath(home))
	if err != nil {
		return errors.New("no pid file; was baf started with `baf start`?")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return err
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		os.Remove(pidPath(home))
		fmt.Println("baf was not running")
		return nil
	}
	for i := 0; i < 300 && syscall.Kill(pid, 0) == nil; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	os.Remove(pidPath(home))
	fmt.Println("baf stopped")
	return nil
}

func pidPath(home string) string { return filepath.Join(home, "baf.pid") }

// wait blocks until every given message id is in this node's mailbox
// (addressed to --as, if given). It keeps waiting while the daemon is still
// starting.
func wait(args []string) error {
	fs := flag.NewFlagSet("wait", flag.ExitOnError)
	as := fs.String("as", os.Getenv("BAF_AGENT"), "")
	timeout := fs.Duration("timeout", 2*time.Minute, "")
	ids, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return errors.New("usage: baf wait [--as AGENT] [--timeout 2m] ID...")
	}
	c, err := client()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(*timeout)
	for {
		items, err := c.Inbox(*as, true)
		if err == nil {
			missing := slices.Clone(ids)
			for _, it := range items {
				missing = slices.DeleteFunc(missing, func(id string) bool { return id == it.Message.ID })
			}
			if len(missing) == 0 {
				fmt.Printf("%d message(s) have arrived\n", len(ids))
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("still not arrived after %s: %s", *timeout, strings.Join(missing, " "))
			}
		} else if !errors.Is(err, daemon.ErrNotRunning) {
			return err
		} else if time.Now().After(deadline) {
			return err
		}
		time.Sleep(time.Second)
	}
}
