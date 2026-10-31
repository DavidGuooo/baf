package door

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"time"
)

// Listeners owns the two HTTP servers and all of their listening addresses.
type Listeners struct {
	public, signin *http.Server
	listeners      []net.Listener
	errors         chan error
}

// Listen binds sign-in only to the supplied Tailscale addresses. Certificates
// come from the local Tailscale API; this does not configure Tailscale Serve.
func (s *Server) Listen(addrs []netip.Addr, signinPort int, publicListen string,
	certificate func(*tls.ClientHelloInfo) (*tls.Certificate, error),
) (*Listeners, error) {
	if len(addrs) == 0 {
		return nil, fmt.Errorf("door sign-in needs this machine's Tailscale addresses")
	}
	if certificate == nil {
		return nil, fmt.Errorf("door sign-in needs Tailscale's certificate provider")
	}
	server := func(h http.Handler) *http.Server {
		return &http.Server{
			Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
			WriteTimeout: 20 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
		}
	}
	ls := &Listeners{
		public: server(s.PublicHandler()), signin: server(s.SigninHandler()),
		errors: make(chan error, len(addrs)+1),
	}
	ls.signin.TLSConfig = &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			// Never ask Tailscale to obtain a certificate for a caller-supplied name.
			if hello.ServerName != signinHost(s.signinURL) {
				return nil, fmt.Errorf("unexpected sign-in TLS name")
			}
			return certificate(hello)
		},
	}
	public, err := net.Listen("tcp", publicListen)
	if err != nil {
		return nil, fmt.Errorf("door public listener %s: %w", publicListen, err)
	}
	ls.listeners = append(ls.listeners, public)
	for _, addr := range addrs {
		ln, err := net.Listen("tcp", net.JoinHostPort(addr.String(), strconv.Itoa(signinPort)))
		if err != nil {
			for _, ln := range ls.listeners {
				ln.Close()
			}
			return nil, fmt.Errorf("door sign-in listener %s:%d: %w", addr, signinPort, err)
		}
		ls.listeners = append(ls.listeners, ln)
	}
	go func() { ls.report(ls.public.Serve(public)) }()
	for _, ln := range ls.listeners[1:] {
		go func() { ls.report(ls.signin.ServeTLS(ln, "", "")) }()
	}
	return ls, nil
}

func (ls *Listeners) report(err error) {
	if err != nil && err != http.ErrServerClosed {
		ls.errors <- err
	}
}

// Errors reports an unexpected listener failure so the daemon can stop cleanly.
func (ls *Listeners) Errors() <-chan error { return ls.errors }

// Close stops both listeners, allowing active requests up to five seconds.
func (ls *Listeners) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ls.public.Shutdown(ctx)
	ls.signin.Shutdown(ctx)
	// Close also ends connections that outlasted the shutdown deadline.
	ls.public.Close()
	ls.signin.Close()
	for _, ln := range ls.listeners {
		ln.Close()
	}
}

func signinHost(raw string) string {
	u, _ := url.Parse(raw)
	return u.Hostname()
}
