package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/work-frame/goflow/internal/ratelimit"
)

// limitedFixture is the auth fixture with a real limiter counting in
// memory, and a log the tests can read.
type limitedFixture struct {
	*authFixture
	logs *bytes.Buffer
}

func newLimitedFixture(t *testing.T, clientIPHeader string) *limitedFixture {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	f := newAuthFixture(t, testHasher, logger)
	f.router = NewRouter(Deps{
		Users: f.users, Sessions: f.sessions, Hasher: testHasher, Logger: logger, DB: okPinger, Jobs: newTestStore(),
		Limiter: ratelimit.New(nil, logger), ClientIPHeader: clientIPHeader,
	})
	return &limitedFixture{authFixture: f, logs: &logs}
}

// loginFrom logs in from the connection address remote, with extra headers.
func (f *limitedFixture) loginFrom(t *testing.T, remote, email, pw string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, loginPath, strings.NewReader(`{"email":"`+email+`","password":"`+pw+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remote
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func expect429(t *testing.T, rec *httptest.ResponseRecorder, what string) {
	t.Helper()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("%s: status %d, body %s; want 429", what, rec.Code, rec.Body)
	}
	if s, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || s < 1 {
		t.Errorf("%s: Retry-After = %q, want a number of seconds", what, rec.Header().Get("Retry-After"))
	}
	if !strings.Contains(rec.Body.String(), "too many attempts") {
		t.Errorf("%s: body %s", what, rec.Body)
	}
}

// Ten attempts per email in 15 minutes, from any number of addresses.
// The eleventh is refused even with the right password: by then it can't
// be told from a guess that got lucky.
func TestLoginIsLimitedPerEmail(t *testing.T) {
	f := newLimitedFixture(t, "")
	for i := range loginPerEmail.Limit {
		remote := "198.51.100." + strconv.Itoa(i+1) + ":4000" // a new address each time
		if rec := f.loginFrom(t, remote, "dan@example.com", "wrong password!", nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d, want 401", i+1, rec.Code)
		}
	}
	expect429(t, f.loginFrom(t, "198.51.100.200:4000", "dan@example.com", password, nil), "11th attempt with the right password")
}

// " Dan@Example.COM" is the same account as "dan@example.com", so it must
// be the same counter, or changing the case would reset the limit.
func TestEmailVariantsShareOneCounter(t *testing.T) {
	f := newLimitedFixture(t, "")
	variants := []string{"dan@example.com", "DAN@example.com", " dan@EXAMPLE.com", "Dan@Example.Com "}
	for i := range loginPerEmail.Limit {
		f.loginFrom(t, "198.51.100."+strconv.Itoa(i+1)+":4000", variants[i%len(variants)], "wrong password!", nil)
	}
	expect429(t, f.loginFrom(t, "198.51.100.99:4000", "dAn@eXample.com", "wrong password!", nil), "a new spelling of a used-up email")
}

// An email nobody registered is limited exactly like a real one. If it
// weren't, a 429 would mean "this account exists".
func TestUnknownEmailsAreLimitedLikeRealOnes(t *testing.T) {
	f := newLimitedFixture(t, "")
	answers := map[string]*httptest.ResponseRecorder{}
	for _, email := range []string{"dan@example.com", "nobody@example.com"} {
		var last *httptest.ResponseRecorder
		for i := range loginPerEmail.Limit + 1 {
			last = f.loginFrom(t, "198.51.100."+strconv.Itoa(i+1)+":4000", email, "wrong password!", nil)
		}
		answers[email] = last
	}
	real, unknown := answers["dan@example.com"], answers["nobody@example.com"]
	if real.Code != unknown.Code || real.Body.String() != unknown.Body.String() {
		t.Fatalf("11th attempt: registered %d %s, unknown %d %s; must be identical",
			real.Code, real.Body, unknown.Code, unknown.Body)
	}
	expect429(t, unknown, "11th attempt for an unknown email")
}

// One address can't try more than 30 logins in 15 minutes, whatever the
// emails. Malformed emails count too.
func TestLoginIsLimitedPerAddress(t *testing.T) {
	f := newLimitedFixture(t, "")
	for i := range loginPerIP.Limit {
		email := "user" + strconv.Itoa(i) + "@example.com"
		if i%5 == 0 {
			email = "not-an-email"
		}
		if rec := f.loginFrom(t, "203.0.113.7:5000", email, "wrong password!", nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d, want 401", i+1, rec.Code)
		}
	}
	expect429(t, f.loginFrom(t, "203.0.113.7:6000", "someone@example.com", "x", nil), "31st attempt from one address (new port)")
	if rec := f.loginFrom(t, "203.0.113.8:5000", "dan@example.com", password, nil); rec.Code != http.StatusOK {
		t.Errorf("another address: status %d, want 200", rec.Code)
	}
}

// With a trusted header configured, the address comes from it (on Render,
// Cloudflare's CF-Connecting-IP), not from the proxy that connected.
func TestAddressComesFromTheTrustedHeader(t *testing.T) {
	f := newLimitedFixture(t, "CF-Connecting-IP")
	for i := range loginPerIP.Limit {
		h := map[string]string{"CF-Connecting-IP": "203.0.113.7"}
		f.loginFrom(t, "[::1]:38350", "user"+strconv.Itoa(i)+"@example.com", "x", h)
	}
	expect429(t, f.loginFrom(t, "[::1]:38350", "a@example.com", "x", map[string]string{"CF-Connecting-IP": "203.0.113.7"}), "same CF-Connecting-IP")
	// Every request arrives from Render's local proxy at [::1]. Counting
	// that address would put all users in one bucket.
	if rec := f.loginFrom(t, "[::1]:38350", "dan@example.com", password, map[string]string{"CF-Connecting-IP": "203.0.113.8"}); rec.Code != http.StatusOK {
		t.Errorf("another client through the same proxy: status %d, want 200", rec.Code)
	}
}

// Without a trusted header, client-sent headers change nothing: a laptop
// has no proxy cleaning them, so they are whatever the client says.
func TestHeadersAreIgnoredWithoutATrustedProxy(t *testing.T) {
	f := newLimitedFixture(t, "")
	for i := range loginPerIP.Limit {
		fake := map[string]string{"CF-Connecting-IP": "192.0.2." + strconv.Itoa(i+1), "X-Forwarded-For": "192.0.2." + strconv.Itoa(i+1)}
		f.loginFrom(t, "203.0.113.7:5000", "user"+strconv.Itoa(i)+"@example.com", "x", fake)
	}
	expect429(t, f.loginFrom(t, "203.0.113.7:5000", "a@example.com", "x", map[string]string{"CF-Connecting-IP": "192.0.2.250"}), "a new fake address each time")
}

func TestClientIPGroupsIPv6By64(t *testing.T) {
	h := &handler{}
	key := func(remote string) string {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = remote
		return h.clientIP(r)
	}
	a, b := key("[2001:db8:1:2:aaaa::1]:443"), key("[2001:db8:1:2:ffff:ffff:ffff:ffff]:443")
	if a != b || a != "2001:db8:1:2::/64" {
		t.Errorf("same /64 gave %q and %q, want both 2001:db8:1:2::/64", a, b)
	}
	if c := key("[2001:db8:1:3::1]:443"); c == a {
		t.Error("a different /64 got the same key")
	}
	if v4 := key("203.0.113.9:443"); v4 != "203.0.113.9" {
		t.Errorf("IPv4 key %q", v4)
	}
	h.clientIPHeader = "CF-Connecting-IP"
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "[::1]:1"
	r.Header.Set("CF-Connecting-IP", "not an address")
	if got := h.clientIP(r); got != "::/64" {
		t.Errorf("garbage in the header gave %q, want the connection's address", got)
	}
}

func TestPasswordChangeIsLimitedPerAccount(t *testing.T) {
	f := newLimitedFixture(t, "")
	token := f.mustLogin(t)
	change := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/password",
			strings.NewReader(`{"current_password":"wrong password!","new_password":"another long password"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		return rec
	}
	for i := range passwordPerAccount.Limit {
		if rec := change(); rec.Code != http.StatusForbidden {
			t.Fatalf("attempt %d: status %d, want 403", i+1, rec.Code)
		}
	}
	expect429(t, change(), "6th current-password guess")
}

func TestTicketsAreLimitedPerAccount(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	router := signedIn(Deps{Jobs: newTestStore(), Live: newFakeLive(), DB: okPinger, Logger: logger, Limiter: ratelimit.New(nil, logger)})
	for i := range ticketsPerAccount.Limit {
		if rec := do(t, router, http.MethodPost, "/api/v1/events/ticket", ""); rec.Code != http.StatusCreated {
			t.Fatalf("ticket %d: status %d", i+1, rec.Code)
		}
	}
	expect429(t, do(t, router, http.MethodPost, "/api/v1/events/ticket", ""), "31st ticket")
}

// The log says which limit was hit, never whose email or address it was.
func TestRateLimitLogsNoEmailOrAddress(t *testing.T) {
	f := newLimitedFixture(t, "")
	for range loginPerEmail.Limit + 1 {
		f.loginFrom(t, "203.0.113.77:5000", "dan@example.com", "wrong password!", nil)
	}
	out := f.logs.String()
	if !strings.Contains(out, "rate limited") || !strings.Contains(out, "rule=login_email") {
		t.Fatalf("no rate-limit log line: %s", out)
	}
	if strings.Contains(out, "dan@example.com") || strings.Contains(out, "203.0.113.77") {
		t.Errorf("log contains the email or the address:\n%s", out)
	}
}

// Without a limiter (most handler tests), nothing is limited.
func TestNoLimiterMeansNoLimits(t *testing.T) {
	f := newAuthFixture(t, testHasher, discardLogger())
	for range loginPerEmail.Limit + 2 {
		f.login(t, "dan@example.com", "wrong password!")
	}
	if rec := f.login(t, "dan@example.com", password); rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
}
