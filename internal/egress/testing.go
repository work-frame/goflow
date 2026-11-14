package egress

import (
	"context"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"testing"
	"time"
)

// TestOptions loosen NewTestClient for a test server on this machine.
type TestOptions struct {
	// Allow adds ranges to the allowed addresses, such as 127.0.0.1/32.
	Allow []netip.Prefix
	// Port replaces 443: a test server listens on a random port.
	Port string
	// Hosts sends a name to a test server: a URL such as
	// https://example.com/hook (port 443, as webhooks require) reaches the
	// server at the given address and port. The address must be allowed
	// (Allow). example.com matches httptest's certificate.
	Hosts map[string]netip.AddrPort
	// RootCAs trusts the test server's certificate.
	RootCAs *x509.CertPool
	// Timeout replaces Timeout when set.
	Timeout time.Duration
}

// testResolver answers for Hosts and refuses every other name: a test
// never looks up a real name.
type testResolver map[string]netip.AddrPort

func (r testResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if ap, ok := r[host]; ok {
		return []netip.Addr{ap.Addr()}, nil
	}
	return nil, fmt.Errorf("egress: test client: no address for %q", host)
}

// NewTestClient is NewClient for tests: it can reach a local test server.
// It needs a testing.TB, so production code can't call it, and
// TestOnlyTestsUseTheTestClient (cmd/api) checks no program does.
func NewTestClient(t testing.TB, o TestOptions) *http.Client {
	t.Helper()
	allow := func(a netip.Addr) bool {
		for _, p := range o.Allow {
			if p.Contains(a.Unmap()) {
				return true
			}
		}
		return Allowed(a)
	}
	timeout := o.Timeout
	if timeout == 0 {
		timeout = Timeout
	}
	port := o.Port
	if port == "" {
		port = Port
	}
	nd := netDialer(allow)
	dial := nd.DialContext
	if len(o.Hosts) > 0 {
		// The dialer checked addr:443; the test server listens elsewhere.
		ports := map[netip.Addr]uint16{}
		for _, ap := range o.Hosts {
			ports[ap.Addr()] = ap.Port()
		}
		dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			ap, err := netip.ParseAddrPort(address)
			if err == nil {
				if p, ok := ports[ap.Addr()]; ok {
					address = netip.AddrPortFrom(ap.Addr(), p).String()
				}
			}
			return nd.DialContext(ctx, network, address)
		}
	}
	var res resolver = net.DefaultResolver
	if len(o.Hosts) > 0 {
		res = testResolver(o.Hosts)
	}
	c := newClient(&dialer{resolve: res, allow: allow, port: port, dial: dial}, o.RootCAs, timeout)
	t.Cleanup(c.CloseIdleConnections)
	return c
}
