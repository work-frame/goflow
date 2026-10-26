package database

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"

	"github.com/work-frame/goflow/internal/auth"
)

var testHasher = auth.Hasher{Cost: bcrypt.MinCost}

func newTestUserStore(t *testing.T) *UserStore {
	t.Helper()
	return NewUserStore(newMigratedTestDB(t))
}

func mustNewUser(t *testing.T, email string) *auth.User {
	t.Helper()
	u, err := auth.NewUser(email, "correct horse battery", testHasher)
	if err != nil {
		t.Fatalf("auth.NewUser(%q): %v", email, err)
	}
	return u
}

func TestUserStoreCreateAndGetRoundTrip(t *testing.T) {
	s := newTestUserStore(t)
	ctx := context.Background()
	u := mustNewUser(t, "dan@example.com")

	if err := s.Create(ctx, u); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := s.GetByEmail(ctx, "dan@example.com")
	if err != nil {
		t.Fatalf("GetByEmail: %v", err)
	}
	if *got != *u {
		t.Errorf("round trip changed the user:\n got  %+v\n want %+v", got, u)
	}
}

func TestUserStoreGetMissing(t *testing.T) {
	s := newTestUserStore(t)
	if _, err := s.GetByEmail(context.Background(), "nobody@example.com"); !errors.Is(err, auth.ErrUserNotFound) {
		t.Fatalf("err = %v, want auth.ErrUserNotFound", err)
	}
}

func TestUserStoreRejectsTakenEmail(t *testing.T) {
	s := newTestUserStore(t)
	ctx := context.Background()

	if err := s.Create(ctx, mustNewUser(t, "dan@example.com")); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	// NewUser lowercases, so this is the same account.
	err := s.Create(ctx, mustNewUser(t, "Dan@Example.com"))
	if !errors.Is(err, auth.ErrEmailTaken) {
		t.Fatalf("second Create err = %v, want auth.ErrEmailTaken", err)
	}
}

func TestUserStoreDuplicateIDIsNotEmailTaken(t *testing.T) {
	s := newTestUserStore(t)
	ctx := context.Background()

	first := mustNewUser(t, "a@example.com")
	if err := s.Create(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := mustNewUser(t, "b@example.com")
	second.ID = first.ID

	err := s.Create(ctx, second)
	if err == nil || errors.Is(err, auth.ErrEmailTaken) {
		t.Fatalf("err = %v, want a plain error: the email is free, the ID is not", err)
	}
}

func TestUsersTableConstraints(t *testing.T) {
	db := newMigratedTestDB(t)
	ctx := context.Background()

	const insert = `INSERT INTO users (id, email, password_hash, created_at) VALUES ($1, $2, $3, $4)`
	now := time.Now().UTC()
	hash, err := testHasher.Hash("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, id, email, hash string
	}{
		{"id not 32 hex chars", "not-hex", "a@example.com", hash},
		{"uppercase email", strings.Repeat("b", 32), "Dan@example.com", hash},
		{"empty email", strings.Repeat("c", 32), "", hash},
		{"plain-text password", strings.Repeat("d", 32), "d@example.com", "correct horse battery"},
		{"truncated hash", strings.Repeat("e", 32), "e@example.com", hash[:30]},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, insert, tc.id, tc.email, tc.hash, now)

			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
				t.Fatalf("err = %v, want check_violation (23514)", err)
			}
		})
	}
}
