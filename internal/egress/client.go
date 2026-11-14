package egress

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"syscall"
	"time"
)

// Timeout bounds a whole request: connecting, TLS, sending, and reading the
// response to its end. A webhook that takes longer has failed.
const Timeout = 10 * time.Second

// Port is the only port webhooks may use (Phase 15, decision 3b).
const Port = "443"

// resolver is the part of *net.Resolver the dialer uses; tests replace it.
type resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// dialer connects only to allowed addresses on the allowed port.
type dialer struct {
	resolve resolver
	allow   func(netip.Addr) bool
	port    string
	// dial opens the connection to an IP address that was already checked.
	dial func(ctx context.Context, network, address string) (net.Conn, error)
}

// DialContext resolves host once, refuses it if ANY of its addresses is not
// allowed, then connects to the checked addresses themselves, in order,
// until one answers. The name is never looked up again, so the address
// connected to is one that passed the check. A name that answers with one
// public and one private address is refused as a whole: it is either
// misconfigured or an attack, and either way not something to half-trust.
func (d *dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("egress: %w", err)
	}
	if port != d.port {
		return nil, &BlockedError{Reason: "port " + port + " is not allowed, only " + d.port}
	}
	portNum, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("egress: port: %w", err)
	}

	var addrs []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{ip}
	} else {
		addrs, err = d.resolve.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("egress: resolve: %w", err)
		}
	}
	if len(addrs) == 0 {
		return nil, errors.New("egress: resolve: no addresses")
	}
	for _, a := range addrs {
		if !d.allow(a) {
			return nil, blockedAddr(a.Unmap())
		}
	}

	var lastErr error
	for _, a := range addrs {
		conn, err := d.dial(ctx, "tcp", netip.AddrPortFrom(a.Unmap(), uint16(portNum)).String())
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}

// netDialer is the last line: the socket itself refuses an address that
// isn't allowed, whatever the code above it did.
func netDialer(allow func(netip.Addr) bool) *net.Dialer {
	return &net.Dialer{
		Timeout: 5 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("egress: %w", err)
			}
			if !allow(ap.Addr()) {
				return blockedAddr(ap.Addr().Unmap())
			}
			return nil
		},
	}
}

func newClient(d *dialer, roots *x509.CertPool, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                  nil, // never HTTP_PROXY: a proxy would connect for us, unchecked
			DialContext:            d.DialContext,
			ForceAttemptHTTP2:      true,
			TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
			TLSHandshakeTimeout:    5 * time.Second,
			ResponseHeaderTimeout:  timeout,
			MaxResponseHeaderBytes: 16 << 10,
			MaxIdleConns:           10,
			IdleConnTimeout:        30 * time.Second,
		},
		// Never follow a redirect: the 3xx response itself is the answer
		// (Phase 15, decision 3a).
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// NewClient returns the client for user-chosen destinations: public
// addresses on port 443 only, certificates checked against the system's
// roots, no proxy, no redirects, Timeout for the whole request.
func NewClient() *http.Client {
	nd := netDialer(Allowed)
	return newClient(&dialer{resolve: net.DefaultResolver, allow: Allowed, port: Port, dial: nd.DialContext}, nil, Timeout)
}
