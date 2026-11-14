// Package egress makes the HTTP requests GoFlow sends to addresses its
// users choose (webhooks, Phase 15). Such a request must never reach
// anything that only GoFlow's own network can reach: this machine, a
// private network, a cloud provider's metadata service. That attack is
// called server-side request forgery (SSRF).
//
// The check happens when connecting, on the exact IP address the
// connection goes to: a check when the job is created can't hold, because
// the name's DNS answer can change after it (DNS rebinding).
package egress

import (
	"fmt"
	"net/netip"
)

// blocked are the ranges Allowed refuses beyond what netip's own methods
// recognise (loopback, private, link-local, multicast, unspecified).
var blocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT, shared address space
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, and 255.255.255.255
	netip.MustParsePrefix("::/96"),           // IPv4-compatible IPv6 (deprecated): could wrap 127.0.0.1
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64: wraps an IPv4 address, maybe a private one
	netip.MustParsePrefix("64:ff9b:1::/48"),  // local-use NAT64
	netip.MustParsePrefix("100::/64"),        // discard-only
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2002::/16"),       // 6to4: wraps an IPv4 address, maybe a private one
	netip.MustParsePrefix("fec0::/10"),       // site-local (deprecated)
}

// Allowed reports whether GoFlow may connect to addr on a user's behalf:
// only public unicast addresses. An IPv4 address written as IPv6
// (::ffff:127.0.0.1) is judged as the IPv4 address it is.
func Allowed(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	addr = addr.Unmap()
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() ||
		addr.IsMulticast() || addr.IsUnspecified() {
		return false
	}
	for _, p := range blocked {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// BlockedError is returned when a destination isn't allowed: its port, or
// one of its host's addresses. Retrying can't help. The message names the
// address but never the URL, whose path may hold a token.
type BlockedError struct {
	Reason string
}

func (e *BlockedError) Error() string { return "egress: blocked: " + e.Reason }

func blockedAddr(addr netip.Addr) error {
	return &BlockedError{Reason: fmt.Sprintf("%s is a private, loopback, link-local or reserved address", addr)}
}
