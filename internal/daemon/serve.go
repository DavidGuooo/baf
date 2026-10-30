package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/DavidGuooo/baf/internal/config"
	"github.com/DavidGuooo/baf/internal/mailbox"
)

// SocketPath is where the daemon listens, inside baf's home directory.
func SocketPath(home string) string { return filepath.Join(home, "baf.sock") }

// Serve runs the mailbox and its local API until ctx is cancelled. On the
// way out it stops running agents, records how their runs ended and queues
// their Results before returning (design notes section 7, "cleanup before
// quitting").
func Serve(ctx context.Context, home, node string, cfg config.Config) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	adapters, err := cfg.Adapters(home)
	if err != nil {
		return err
	}
	socket := SocketPath(home)
	ln, err := listenSocket(socket)
	if err != nil {
		return err
	}

	mb, err := mailbox.Open(mailbox.Options{
		Path:   filepath.Join(home, "mailbox.db"),
		Node:   node,
		Agents: adapters,
		RunEnv: runEnv(home),
	})
	if err != nil {
		ln.Close()
		return err
	}
	defer mb.Close()

	var names []string
	for n := range adapters {
		names = append(names, n)
	}
	srv := &http.Server{Handler: Handler(mb, names), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("local API stopped", "err", err)
		}
	}()
	slog.Info("baf is running", "node", node, "agents", strings.Join(names, ","), "socket", socket)

	// Run returns only after every agent run it started has been recorded.
	mb.Run(ctx)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	os.Remove(socket)
	slog.Info("baf stopped")
	return nil
}

// listenSocket listens on a Unix socket only its owner can use. A socket file
// left behind by a crash is removed; one that still answers means another
// daemon is running.
func listenSocket(path string) (net.Listener, error) {
	if c, err := net.Dial("unix", path); err == nil {
		c.Close()
		return nil, fmt.Errorf("another baf daemon is already listening on %s", path)
	}
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// runEnv tells each agent run who it is and how to reach this daemon, and
// puts this baf binary on its PATH, so the agent can run `baf send` itself.
func runEnv(home string) func(string) []string {
	path := os.Getenv("PATH")
	if exe, err := os.Executable(); err == nil {
		path = filepath.Dir(exe) + string(os.PathListSeparator) + path
	}
	return func(agentName string) []string {
		return []string{
			"BAF_AGENT=" + agentName,
			"BAF_HOME=" + home,
			"PATH=" + path,
		}
	}
}
