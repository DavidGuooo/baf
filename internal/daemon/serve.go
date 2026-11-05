package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/DavidGuooo/baf/internal/config"
	"github.com/DavidGuooo/baf/internal/door"
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
	if err := cfg.Door.Validate(); err != nil {
		return err
	}
	if cfg.Door.Enabled && (cfg.Door.SigninPort == cfg.Port || (cfg.Port == 0 && cfg.Door.SigninPort == transport.DefaultPort)) {
		return errors.New("door.signin_port must differ from baf's mailbox port")
	}
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	home, err := filepath.Abs(home)
	if err != nil {
		return err
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	commandHome := home
	if home == filepath.Join(userHome, ".baf") {
		commandHome = ""
	}
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
	if cfg.Door.Enabled {
		u, _ := url.Parse(cfg.Door.PublicURL)
		if u.Hostname() != self.DNSName {
			return fmt.Errorf("door.public_url must name this Relay's Tailscale DNS name: %s", self.DNSName)
		}
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
	defer localLn.Close()

	var doorIdentity mailbox.DoorIdentity
	if cfg.Door.Enabled {
		doorIdentity = ts
	}
	mb, err := mailbox.Open(mailbox.Options{
		Path:         filepath.Join(home, "mailbox.db"),
		Node:         node,
		Agents:       adapters,
		RunEnv:       runEnv(home),
		NewThreadDir: newThreadDir(cfg, home, userHome, runtime.GOOS),
		UserHome:     userHome,
		BafBinary:    binary,
		BafHome:      commandHome,
		Transport:    &transport.Client{Lookup: ts.Lookup, FindRelay: ts.Relay, Port: port},
		Doorbell:     &transport.Doorbells{From: node, Bells: bells},
		Door:         doorIdentity,
		GiveUpAfter:  cfg.GiveUp(),
	})
	if err != nil {
		localLn.Close()
		return err
	}
	defer mb.Close()

	var publicDoor *door.Server
	var doorListeners *door.Listeners
	if cfg.Door.Enabled {
		signinURL := "https://" + net.JoinHostPort(self.DNSName, strconv.Itoa(cfg.Door.SigninPort))
		publicDoor, err = door.Open(door.Options{
			Path: filepath.Join(home, "door.db"), PublicURL: cfg.Door.PublicURL, SigninURL: signinURL,
			Policy: mb, Identity: ts, Mailbox: mb,
		})
		if err != nil {
			return fmt.Errorf("opening Public door: %w", err)
		}
		defer publicDoor.Close()
		doorListeners, err = publicDoor.Listen(self.Addrs, cfg.Door.SigninPort, cfg.Door.PublicListen, ts.GetCertificate)
		if err != nil {
			return err
		}
		defer doorListeners.Close()
		go func() {
			select {
			case err := <-doorListeners.Errors():
				stop(fmt.Errorf("Public door listener stopped: %w", err))
			case <-ctx.Done():
			}
		}()
	}

	var names []string
	for n := range adapters {
		names = append(names, n)
	}
	local := &http.Server{Handler: HandlerWithDoor(mb, names, publicDoor), ReadHeaderTimeout: 10 * time.Second}
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

	if publicDoor != nil {
		done := make(chan struct{})
		go func() {
			defer close(done)
			publicDoor.RunEvents(ctx)
		}()
		defer func() { <-done }()
	}
	// Run returns only after every agent run it started has been recorded.
	mb.Run(ctx)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	peers.Shutdown(shutdownCtx)
	local.Shutdown(shutdownCtx)
	os.Remove(socket)
	slog.Info("baf stopped")
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) && !errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
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
