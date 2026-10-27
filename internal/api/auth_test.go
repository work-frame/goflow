package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/work-frame/goflow/internal/auth"
)

// testHasher keeps bcrypt fast: cost 4 takes about 2 ms, cost 12 about 450 ms.
var testHasher = auth.Hasher{Cost: bcrypt.MinCost}

// memUsers is an in-memory UserStore. Like PostgreSQL's UNIQUE constraint,
// it refuses a second user with the same email.
type memUsers struct {
	mu       sync.Mutex
	byEmail  map[string]*auth.User
	sessions *memSessions // set by newMemSessions, so ChangePassword can end sessions
}

func newMemUsers() *memUsers { return &memUsers{byEmail: map[string]*auth.User{}} }

func (s *memUsers) Create(_ context.Context, u *auth.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byEmail[u.Email]; ok {
		return auth.ErrEmailTaken
	}
	cp := *u
	s.byEmail[u.Email] = &cp
	return nil
}

// errUsers fails every call with the same error.
type errUsers struct{ err error }

func (s errUsers) Create(context.Context, *auth.User) error { return s.err }

const registerPath = "/api/v1/auth/register"

func TestRegister(t *testing.T) {
	users := newMemUsers()
	router := NewRouter(Deps{Users: users, Hasher: testHasher, Logger: discardLogger()})

	rec := do(t, router, http.MethodPost, registerPath,
		`{"email":"  Dan@Example.com ","password":"correct horse battery"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body %s", rec.Code, rec.Body)
	}

	body := rec.Body.String()
	if strings.Contains(body, "password") || strings.Contains(body, "$2a$") {
		t.Errorf("response leaks the password or its hash: %s", body)
	}

	got := decode[map[string]any](t, rec)
	if got["email"] != "dan@example.com" {
		t.Errorf("email = %v, want dan@example.com", got["email"])
	}
	if id, _ := got["id"].(string); len(id) != 32 {
		t.Errorf("id = %v, want 32 hex characters", got["id"])
	}
	if _, ok := got["created_at"]; !ok {
		t.Error("response has no created_at")
	}

	stored := users.byEmail["dan@example.com"]
	if stored == nil {
		t.Fatal("user was not stored")
	}
	if err := testHasher.Compare(stored.PasswordHash, "correct horse battery"); err != nil {
		t.Errorf("stored hash does not match the password: %v", err)
	}
}

func TestRegisterSameEmailTwiceIs409(t *testing.T) {
	router := NewRouter(Deps{Users: newMemUsers(), Hasher: testHasher, Logger: discardLogger()})

	first := do(t, router, http.MethodPost, registerPath, `{"email":"dan@example.com","password":"correct horse battery"}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("first register: status %d, body %s", first.Code, first.Body)
	}
	second := do(t, router, http.MethodPost, registerPath, `{"email":"DAN@example.com","password":"another long password"}`)
	if second.Code != http.StatusConflict {
		t.Fatalf("second register: status %d, want 409, body %s", second.Code, second.Body)
	}
}

func TestRegisterValidation(t *testing.T) {
	tests := []struct {
		name, body string
		want       int
		wantMsg    string
	}{
		{"bad email", `{"email":"nope","password":"correct horse battery"}`, 400, "email is invalid"},
		{"missing email", `{"password":"correct horse battery"}`, 400, "email is invalid"},
		{"short password", `{"email":"dan@example.com","password":"short"}`, 400, "at least 12 characters"},
		{"missing password", `{"email":"dan@example.com"}`, 400, "at least 12 characters"},
		{"long password", `{"email":"dan@example.com","password":"` + strings.Repeat("a", 73) + `"}`, 400, "at most 72 bytes"},
		{"unknown field", `{"email":"dan@example.com","password":"correct horse battery","admin":true}`, 400, "unknown field"},
		{"empty body", ``, 415, "Content-Type"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			router := NewRouter(Deps{Users: newMemUsers(), Hasher: testHasher, Logger: discardLogger()})
			rec := do(t, router, http.MethodPost, registerPath, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d, body %s", rec.Code, tc.want, rec.Body)
			}
			if msg := decode[map[string]string](t, rec)["error"]; !strings.Contains(msg, tc.wantMsg) {
				t.Errorf("error = %q, want it to contain %q", msg, tc.wantMsg)
			}
		})
	}
}

func TestRegisterStoreErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"timeout", context.DeadlineExceeded, http.StatusServiceUnavailable},
		{"anything else", errors.New("connection reset"), http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			router := NewRouter(Deps{Users: errUsers{err: tc.err}, Hasher: testHasher, Logger: discardLogger()})
			rec := do(t, router, http.MethodPost, registerPath, `{"email":"dan@example.com","password":"correct horse battery"}`)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d, body %s", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

// The brief's rule "never log passwords", as a test. The 500 path is the one
// that logs the most: the request line plus the full internal error.
func TestRegisterNeverLogsThePassword(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	router := NewRouter(Deps{Users: errUsers{err: errors.New("disk on fire")}, Hasher: testHasher, Logger: logger})

	const password = "hunter2-but-longer"
	rec := do(t, router, http.MethodPost, registerPath, `{"email":"dan@example.com","password":"`+password+`"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(logs.String(), "disk on fire") {
		t.Fatalf("expected the internal error in the logs, got: %s", logs.String())
	}
	if strings.Contains(logs.String(), password) {
		t.Errorf("logs contain the password: %s", logs.String())
	}
}

// Added for login (Task 17b): the rest of the UserStore interface.

func (s *memUsers) GetByEmail(_ context.Context, email string) (*auth.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.byEmail[email]
	if !ok {
		return nil, auth.ErrUserNotFound
	}
	cp := *u
	return &cp, nil
}

func (s *memUsers) UpdatePasswordHash(_ context.Context, id, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.byEmail {
		if u.ID == id {
			u.PasswordHash = hash
			return nil
		}
	}
	return auth.ErrUserNotFound
}

func (s errUsers) GetByEmail(context.Context, string) (*auth.User, error) { return nil, s.err }

func (s errUsers) UpdatePasswordHash(context.Context, string, string) error { return s.err }

// Added for session hardening (Task 19).

func (s *memUsers) ChangePassword(ctx context.Context, userID, hash string, keepTokenHash []byte) (int64, error) {
	if err := s.UpdatePasswordHash(ctx, userID, hash); err != nil {
		return 0, err
	}
	if s.sessions == nil {
		return 0, nil
	}
	return s.sessions.deleteForUser(userID, keepTokenHash), nil
}

func (s errUsers) ChangePassword(context.Context, string, string, []byte) (int64, error) {
	return 0, s.err
}
