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
	"strconv"
	"strings"
	"time"

	"github.com/DavidGuooo/baf/internal/config"
	"github.com/DavidGuooo/baf/internal/mailbox"
	"github.com/DavidGuooo/baf/internal/tailnet"
	"github.com/DavidGuooo/baf/internal/transport"
)

// SocketPath is where the daemon listens, inside baf's home directory.
func SocketPath(home string) string { return filepath.Join(home, "baf.sock") }

// Serve runs the mailbox, its local API and its tailnet listener until ctx
// is cancelled. On the way out it stops running agents, records how their
// runs ended and queues their Results before returning.
func Serve(ctx context.Context, home string, cfg config.Config, ts *tailnet.Client) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	adapters, err := cfg.Adapters(home)
	if err != nil {
		return err
	}
	bells, err := cfg.Bells()
	if err != nil {
		return err
	}

	self, err := waitForTailscale(ctx, ts, time.Minute)
	if err != nil {
		return fmt.Errorf("asking Tailscale about this machine: %w", err)
	}
	node := self.Node
	if cfg.Node != "" {
		node = cfg.Node
	}
	port := cfg.Port
	if port == 0 {
		port = transport.DefaultPort
	}

	// Listen only on this machine's Tailscale addresses, so nothing outside
	// the tailnet can even reach the port.
	var peerLns []net.Listener
	for _, a := range self.Addrs {
		ln, err := net.Listen("tcp", net.JoinHostPort(a.String(), strconv.Itoa(port)))
		if err != nil {
			slog.Warn("cannot listen on a Tailscale address", "addr", a, "err", err)
			continue
		}
		peerLns = append(peerLns, ln)
	}
	if len(peerLns) == 0 {
		return fmt.Errorf("could not listen on any Tailscale address on port %d", port)
	}
	defer func() {
		for _, ln := range peerLns {
			ln.Close()
		}
	}()

	socket := SocketPath(home)
	localLn, err := listenSocket(socket)
	if err != nil {
		return err
	}

	var door mailbox.DoorIdentity
	if cfg.Door.Enabled {
		door = ts
	}
	mb, err := mailbox.Open(mailbox.Options{
		Path:        filepath.Join(home, "mailbox.db"),
		Node:        node,
		Agents:      adapters,
		RunEnv:      runEnv(home),
		Transport:   &transport.Client{Lookup: ts.Lookup, Port: port},
		Doorbell:    &transport.Doorbells{From: node, Bells: bells},
		Door:        door,
		GiveUpAfter: cfg.GiveUp(),
	})
	if err != nil {
		localLn.Close()
		return err
	}
	defer mb.Close()

	var names []string
	for n := range adapters {
		names = append(names, n)
	}
	local := &http.Server{Handler: Handler(mb, names), ReadHeaderTimeout: 10 * time.Second}
	peers := &http.Server{Handler: transport.Handler(mb, ts), ReadHeaderTimeout: 10 * time.Second}
	serve := func(srv *http.Server, ln net.Listener) {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listener stopped", "addr", ln.Addr(), "err", err)
		}
	}
	go serve(local, localLn)
	var addrs []string
	for _, ln := range peerLns {
		go serve(peers, ln)
		addrs = append(addrs, ln.Addr().String())
	}
	var bellNodes []string
	for n := range bells {
		bellNodes = append(bellNodes, n)
	}
	slog.Info("baf is running", "node", node, "agents", strings.Join(names, ","),
		"tailnet", strings.Join(addrs, ","), "doorbells", strings.Join(bellNodes, ","), "socket", socket)

	// Run returns only after every agent run it started has been recorded.
	mb.Run(ctx)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	peers.Shutdown(shutdownCtx)
	local.Shutdown(shutdownCtx)
	os.Remove(socket)
	slog.Info("baf stopped")
	return nil
}

// waitForTailscale asks Tailscale about this machine until it answers. A
// machine that has just woken up may still be connecting.
func waitForTailscale(ctx context.Context, ts *tailnet.Client, limit time.Duration) (tailnet.Self, error) {
	deadline := time.Now().Add(limit)
	for {
		tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		self, err := ts.Self(tctx)
		cancel()
		if err == nil && len(self.Addrs) > 0 {
			return self, nil
		}
		if err == nil {
			err = errors.New("tailscale has no address for this machine yet")
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return tailnet.Self{}, err
		}
		slog.Info("waiting for Tailscale", "err", err)
		time.Sleep(2 * time.Second)
	}
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
