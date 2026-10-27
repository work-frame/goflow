package client

import (
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/work-frame/goflow/internal/api"
	"github.com/work-frame/goflow/internal/auth"
	"github.com/work-frame/goflow/internal/job"
)

// oneUser is a UserStore with a single account.
type oneUser struct{ u *auth.User }

func (s oneUser) Create(context.Context, *auth.User) error { return auth.ErrEmailTaken }

func (s oneUser) GetByEmail(_ context.Context, email string) (*auth.User, error) {
	if email != s.u.Email {
		return nil, auth.ErrUserNotFound
	}
	u := *s.u
	return &u, nil
}

func (s oneUser) UpdatePasswordHash(context.Context, string, string) error { return nil }

func (s oneUser) ChangePassword(context.Context, string, string, []byte) (int64, error) {
	return 0, nil
}

// memSessions is a SessionStore that really stores sessions, so login and
// logout can be tested together.
type memSessions struct {
	mu   sync.Mutex
	user *auth.User
	m    map[string]bool
}

func (s *memSessions) Create(_ context.Context, sess *auth.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[string(sess.TokenHash)] = true
	return nil
}

func (s *memSessions) Lookup(_ context.Context, hash []byte) (*auth.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.m[string(hash)] {
		return nil, auth.ErrSessionNotFound
	}
	u := *s.user
	return &u, nil
}

func (s *memSessions) Delete(_ context.Context, hash []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, string(hash))
	return nil
}

func (s *memSessions) DeleteAllForUser(context.Context, string) (int64, error) { return 0, nil }

const testPassword = "correct horse battery"

// newLoginServer runs the real API with one account, dan@example.com.
func newLoginServer(t *testing.T) string {
	t.Helper()
	hasher := auth.Hasher{Cost: bcrypt.MinCost}
	u, err := auth.NewUser("dan@example.com", testPassword, hasher)
	if err != nil {
		t.Fatal(err)
	}
	router := api.NewRouter(api.Deps{
		Jobs:     memStore{m: job.NewMemoryStore()},
		Users:    oneUser{u: u},
		Sessions: &memSessions{user: u, m: map[string]bool{}},
		Hasher:   hasher,
		DB:       alwaysReady{},
		Logger:   slog.New(slog.DiscardHandler),
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestClientLoginAndLogout(t *testing.T) {
	url := newLoginServer(t)
	ctx := context.Background()

	token, expires, err := New(url, "").Login(ctx, "dan@example.com", testPassword)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !auth.ValidTokenFormat(token) || expires.IsZero() {
		t.Fatalf("Login = %q, %v; want a token and an expiry", token, expires)
	}

	signedIn := New(url, token)
	if _, _, err := signedIn.List(ctx, "", ""); err != nil {
		t.Fatalf("List with the new token: %v", err)
	}

	// Logout uses the token it is given, not the one the client was made with.
	if err := New(url, "").Logout(ctx, token); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, _, err := signedIn.List(ctx, "", ""); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("List after logout: err = %v, want ErrUnauthenticated", err)
	}
	if err := New(url, "").Logout(ctx, token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("second Logout: err = %v, want ErrUnauthenticated", err)
	}
}

func TestClientLoginWithWrongPassword(t *testing.T) {
	url := newLoginServer(t)

	_, _, err := New(url, "").Login(context.Background(), "dan@example.com", "wrong horse battery")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
	if strings.Contains(err.Error(), "wrong horse battery") {
		t.Error("the error contains the password")
	}
}
