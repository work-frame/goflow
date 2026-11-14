package egress

import (
	"crypto/x509"
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
	// RootCAs trusts the test server's certificate.
	RootCAs *x509.CertPool
	// Timeout replaces Timeout when set.
	Timeout time.Duration
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
	nd := netDialer(allow)
	c := newClient(&dialer{resolve: net.DefaultResolver, allow: allow, port: o.Port, dial: nd.DialContext}, o.RootCAs, timeout)
	t.Cleanup(c.CloseIdleConnections)
	return c
}
