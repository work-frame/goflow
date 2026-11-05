package api

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/work-frame/goflow/internal/ratelimit"
)

// RateLimiter decides whether one more attempt may go ahead.
// *ratelimit.Limiter satisfies it. nil in Deps means no limits (tests).
type RateLimiter interface {
	Allow(ctx context.Context, rule ratelimit.Rule, subject string) ratelimit.Decision
}

// The limits (Phase 9). Every attempt counts, successful or not: a
// legitimate user rarely logs in 10 times in 15 minutes, and counting
// before the password check means a refused attempt costs no bcrypt work.
var (
	// One address can't spray guesses across many accounts, or make the
	// server spend bcrypt time without end.
	loginPerIP = ratelimit.Rule{Name: "login_ip", Limit: 30, Window: 15 * time.Minute}
	// One account can't be guessed from many addresses. Counted for every
	// well-formed email, registered or not: if only real accounts were
	// counted, a 429 would reveal which emails exist (FAILURES.md, case 39).
	loginPerEmail = ratelimit.Rule{Name: "login_email", Limit: 10, Window: 15 * time.Minute}
	// The current-password check is a password guess too.
	passwordPerAccount = ratelimit.Rule{Name: "password_change", Limit: 5, Window: 15 * time.Minute}
	// Tickets are cheap but each one is a Redis write; the dashboard needs
	// one per reconnect, at most every second or so.
	ticketsPerAccount = ratelimit.Rule{Name: "stream_ticket", Limit: 30, Window: 5 * time.Minute}
)

// limited counts one attempt under rule. If the limit is reached it
// answers 429 with Retry-After and returns true; the caller then stops.
func (h *handler) limited(w http.ResponseWriter, r *http.Request, rule ratelimit.Rule, subject string) bool {
	if h.limiter == nil {
		return false
	}
	d := h.limiter.Allow(r.Context(), rule, subject)
	if d.Allowed {
		return false
	}
	seconds := int(math.Ceil(d.RetryAfter.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	// The rule's name, never the subject: no emails or addresses in logs.
	h.logger.Warn("rate limited", "request_id", RequestID(r.Context()), "rule", rule.Name, "retry_after_s", seconds)
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeError(w, http.StatusTooManyRequests, fmt.Sprintf("too many attempts; try again in %d seconds", seconds))
	return true
}

// clientIP returns the address to count a request against.
//
// With ClientIPHeader set (CF-Connecting-IP on Render), it is that
// header's value: Task 37 showed Cloudflare refuses requests where the
// client sends it, so a value that arrives was set by Cloudflare. Without
// it, or when the header is missing or isn't an IP address (Render's own
// health checks, a laptop), it is the connection's address.
//
// IPv6 addresses count per /64 network. One customer usually gets a whole
// /64, 18 quintillion addresses, and could use a new one per attempt.
func (h *handler) clientIP(r *http.Request) string {
	if h.clientIPHeader != "" {
		if ip := net.ParseIP(strings.TrimSpace(r.Header.Get(h.clientIPHeader))); ip != nil {
			return ipKey(ip)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil {
		return ipKey(ip)
	}
	return "unknown"
}

func ipKey(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}
