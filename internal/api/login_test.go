package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/work-frame/goflow/internal/auth"
)

// memSessions is an in-memory SessionStore keyed by token hash.
type memSessions struct {
	mu    sync.Mutex
	users *memUsers
	byKey map[string]*auth.Session
}

func newMemSessions(users *memUsers) *memSessions {
	return &memSessions{users: users, byKey: map[string]*auth.Session{}}
}

func (s *memSessions) Create(_ context.Context, sess *auth.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *sess
	s.byKey[string(sess.TokenHash)] = &cp
	return nil
}

func (s *memSessions) Lookup(_ context.Context, tokenHash []byte) (*auth.User, error) {
	s.mu.Lock()
	sess, ok := s.byKey[string(tokenHash)]
	s.mu.Unlock()
	if !ok || !time.Now().Before(sess.ExpiresAt) {
		return nil, auth.ErrSessionNotFound
	}
	s.users.mu.Lock()
	defer s.users.mu.Unlock()
	for _, u := range s.users.byEmail {
		if u.ID == sess.UserID {
			cp := *u
			return &cp, nil
		}
	}
	return nil, auth.ErrSessionNotFound
}

func (s *memSessions) Delete(_ context.Context, tokenHash []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byKey, string(tokenHash))
	return nil
}

const (
	loginPath  = "/api/v1/auth/login"
	logoutPath = "/api/v1/auth/logout"
	mePath     = "/api/v1/auth/me"
	password   = "correct horse battery"
)

// authFixture is a router with one registered user, dan@example.com.
type authFixture struct {
	router   http.Handler
	users    *memUsers
	sessions *memSessions
	user     *auth.User
}

func newAuthFixture(t *testing.T, hasher auth.Hasher, logger *slog.Logger) *authFixture {
	t.Helper()
	users := newMemUsers()
	sessions := newMemSessions(users)
	u, err := auth.NewUser("dan@example.com", password, hasher)
	if err != nil {
		t.Fatal(err)
	}
	if err := users.Create(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	router := NewRouter(Deps{Users: users, Sessions: sessions, Hasher: hasher, Logger: logger})
	return &authFixture{router: router, users: users, sessions: sessions, user: u}
}

func (f *authFixture) login(t *testing.T, email, pw string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, f.router, http.MethodPost, loginPath, `{"email":"`+email+`","password":"`+pw+`"}`)
}

func (f *authFixture) mustLogin(t *testing.T) string {
	t.Helper()
	rec := f.login(t, "dan@example.com", password)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status %d, body %s", rec.Code, rec.Body)
	}
	return decode[map[string]string](t, rec)["token"]
}

// withToken sends a request with "Authorization: Bearer <token>".
func withToken(t *testing.T, h http.Handler, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestLogin(t *testing.T) {
	f := newAuthFixture(t, testHasher, discardLogger())

	rec := f.login(t, "  Dan@Example.com ", password)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	body := decode[map[string]string](t, rec)
	token := body["token"]
	if !auth.ValidTokenFormat(token) {
		t.Fatalf("token = %q, not a GoFlow token", token)
	}
	if body["expires_at"] == "" {
		t.Error("response has no expires_at")
	}

	stored, ok := f.sessions.byKey[string(auth.HashToken(token))]
	if !ok {
		t.Fatal("no session stored under the token's hash")
	}
	if stored.UserID != f.user.ID {
		t.Errorf("session belongs to %q, want %q", stored.UserID, f.user.ID)
	}
}

func TestLoginFailuresLookTheSame(t *testing.T) {
	f := newAuthFixture(t, testHasher, discardLogger())

	tests := []struct{ name, email, password string }{
		{"wrong password", "dan@example.com", "wrong horse battery"},
		{"unknown email", "nobody@example.com", password},
		{"malformed email", "not-an-email", password},
		{"empty password", "dan@example.com", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.login(t, tc.email, tc.password)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if got := decode[map[string]string](t, rec)["error"]; got != invalidCredentials {
				t.Errorf("error = %q, want %q", got, invalidCredentials)
			}
		})
	}
	if len(f.sessions.byKey) != 0 {
		t.Errorf("failed logins created %d sessions", len(f.sessions.byKey))
	}
}

// An unknown email must cost about as much time as a wrong password.
// Cost 8 makes a bcrypt compare clearly measurable (milliseconds), while
// skipping it takes microseconds. The bar is low (a quarter) to stay stable
// on a busy machine; without the dummy compare the ratio is near zero.
func TestLoginTimingHidesUnknownEmails(t *testing.T) {
	f := newAuthFixture(t, auth.Hasher{Cost: 8}, discardLogger())
	f.login(t, "nobody@example.com", password) // warm up: builds the dummy hash

	timeLogin := func(email string) time.Duration {
		start := time.Now()
		f.login(t, email, "wrong horse battery")
		return time.Since(start)
	}
	known := timeLogin("dan@example.com")
	unknown := timeLogin("nobody@example.com")

	if unknown < known/4 {
		t.Errorf("unknown email took %v, wrong password %v: the difference reveals which emails exist", unknown, known)
	}
}

func TestLoginUpgradesOldHashes(t *testing.T) {
	f := newAuthFixture(t, testHasher, discardLogger()) // user hashed at cost 4
	stronger := auth.Hasher{Cost: bcrypt.MinCost + 1}
	router := NewRouter(Deps{Users: f.users, Sessions: f.sessions, Hasher: stronger, Logger: discardLogger()})

	rec := do(t, router, http.MethodPost, loginPath, `{"email":"dan@example.com","password":"`+password+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status %d, body %s", rec.Code, rec.Body)
	}

	u, _ := f.users.GetByEmail(context.Background(), "dan@example.com")
	if cost, _ := bcrypt.Cost([]byte(u.PasswordHash)); cost != bcrypt.MinCost+1 {
		t.Errorf("stored hash cost = %d, want %d", cost, bcrypt.MinCost+1)
	}
	if err := stronger.Compare(u.PasswordHash, password); err != nil {
		t.Errorf("upgraded hash no longer matches the password: %v", err)
	}
}

func TestMeAndLogout(t *testing.T) {
	f := newAuthFixture(t, testHasher, discardLogger())
	token := f.mustLogin(t)

	rec := withToken(t, f.router, http.MethodGet, mePath, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("me: status %d, body %s", rec.Code, rec.Body)
	}
	if got := decode[map[string]string](t, rec)["email"]; got != "dan@example.com" {
		t.Errorf("me: email = %q", got)
	}

	if rec := withToken(t, f.router, http.MethodPost, logoutPath, token); rec.Code != http.StatusNoContent {
		t.Fatalf("logout: status %d, body %s", rec.Code, rec.Body)
	}
	// Immediate revocation: the same token is dead right away.
	if rec := withToken(t, f.router, http.MethodGet, mePath, token); rec.Code != http.StatusUnauthorized {
		t.Fatalf("me after logout: status %d, want 401", rec.Code)
	}
}

func TestSessionRequired(t *testing.T) {
	f := newAuthFixture(t, testHasher, discardLogger())
	token := f.mustLogin(t)

	expired, sess := auth.NewSession(f.user.ID, time.Now().Add(-2*auth.SessionTTL))
	f.sessions.Create(context.Background(), sess)

	tests := map[string]string{
		"no header":         "",
		"wrong scheme":      "Basic " + token,
		"bearer, no token":  "Bearer ",
		"malformed token":   "Bearer gfs_short",
		"never issued":      "Bearer gfs_" + strings.Repeat("A", 43),
		"expired":           "Bearer " + expired,
		"token without key": token,
	}
	for name, header := range tests {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, mePath, nil)
			if header != "" {
				req.Header.Set("Authorization", header)
			}
			rec := httptest.NewRecorder()
			f.router.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("401 without a WWW-Authenticate header")
			}
		})
	}

	// The scheme name is case-insensitive.
	req := httptest.NewRequest(http.MethodGet, mePath, nil)
	req.Header.Set("Authorization", "bearer "+token)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("lowercase bearer: status %d, want 200", rec.Code)
	}
}

func TestLoginStoreErrors(t *testing.T) {
	router := NewRouter(Deps{Users: errUsers{err: context.DeadlineExceeded}, Hasher: testHasher, Logger: discardLogger()})
	rec := do(t, router, http.MethodPost, loginPath, `{"email":"dan@example.com","password":"`+password+`"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}

	router = NewRouter(Deps{Users: errUsers{err: errors.New("boom")}, Hasher: testHasher, Logger: discardLogger()})
	rec = do(t, router, http.MethodPost, loginPath, `{"email":"dan@example.com","password":"`+password+`"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// Neither the password nor the token may reach the logs, on any path.
func TestAuthNeverLogsSecrets(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f := newAuthFixture(t, testHasher, logger)

	token := f.mustLogin(t)
	withToken(t, f.router, http.MethodGet, mePath, token)
	withToken(t, f.router, http.MethodPost, logoutPath, token)
	withToken(t, f.router, http.MethodGet, mePath, token) // 401 path
	f.login(t, "dan@example.com", "wrong horse battery")

	for _, secret := range []string{password, "wrong horse battery", token} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("logs contain a secret %q:\n%s", secret, logs.String())
		}
	}
}
