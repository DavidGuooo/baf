package door

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"sync/atomic"
	"testing"
)

func TestListenersUseSeparateRoutesAndCertificate(t *testing.T) {
	h := newHarness(t)
	h.s.signinURL = "https://example.com:8444"
	fixture := httptest.NewTLSServer(nil)
	defer fixture.Close()
	certificate := fixture.TLS.Certificates[0]
	var calls atomic.Int32
	ls, err := h.s.Listen([]netip.Addr{netip.MustParseAddr("127.0.0.1")}, 0, "127.0.0.1:0", func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) { calls.Add(1); return &certificate, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Close()
	roots := x509.NewCertPool()
	roots.AddCert(fixture.Certificate())
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "example.com"}}}
	defer client.CloseIdleConnections()
	for _, tc := range []struct {
		client *http.Client
		url    string
		want   int
	}{
		{http.DefaultClient, "http://" + ls.listeners[0].Addr().String() + "/authorize", 404},
		{http.DefaultClient, "http://" + ls.listeners[0].Addr().String() + "/.well-known/oauth-authorization-server", 200},
		{client, "https://" + ls.listeners[1].Addr().String() + "/authorize", 400}, // no OAuth query
		{client, "https://" + ls.listeners[1].Addr().String() + "/token", 404},
	} {
		resp, err := tc.client.Get(tc.url)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("%s: %d want %d", tc.url, resp.StatusCode, tc.want)
		}
	}
	if calls.Load() == 0 {
		t.Fatal("did not use certificate callback")
	}
	addrs := []string{ls.listeners[0].Addr().String(), ls.listeners[1].Addr().String()}
	ls.Close()
	for _, addr := range addrs {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("listener leaked: %s: %v", addr, err)
		}
		ln.Close()
	}
}

func TestListenerFailureClosesEarlierBinds(t *testing.T) {
	h := newHarness(t)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	_, port, _ := net.SplitHostPort(occupied.Addr().String())
	signinPort, _ := strconv.Atoi(port)
	available, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	publicAddr := available.Addr().String()
	available.Close()
	_, err = h.s.Listen([]netip.Addr{netip.MustParseAddr("127.0.0.1")}, signinPort, publicAddr, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil })
	if err == nil {
		t.Fatal("expected sign-in bind failure")
	}
	ln, err := net.Listen("tcp", publicAddr)
	if err != nil {
		t.Fatal("public listener leaked:", err)
	}
	ln.Close()
}
