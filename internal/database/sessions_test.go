package database

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/work-frame/goflow/internal/auth"
)

// newSessionFixture returns a database with one user, plus stores on it.
func newSessionFixture(t *testing.T) (*sql.DB, *auth.User, *SessionStore) {
	t.Helper()
	db := newMigratedTestDB(t)
	u := mustNewUser(t, "dan@example.com")
	if err := NewUserStore(db).Create(context.Background(), u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return db, u, NewSessionStore(db)
}

func TestSessionStoreCreateAndLookup(t *testing.T) {
	_, u, s := newSessionFixture(t)
	ctx := context.Background()

	token, sess := auth.NewSession(u.ID, time.Now())
	if err := s.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := s.Lookup(ctx, auth.HashToken(token))
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if *got != *u {
		t.Errorf("Lookup returned %+v, want %+v", got, u)
	}
}

func TestSessionStoreLookupUnknown(t *testing.T) {
	_, _, s := newSessionFixture(t)
	_, err := s.Lookup(context.Background(), auth.HashToken("gfs_never-issued"))
	if !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatalf("err = %v, want auth.ErrSessionNotFound", err)
	}
}

func TestSessionStoreLookupExpired(t *testing.T) {
	_, u, s := newSessionFixture(t)
	ctx := context.Background()

	// A session that started two days ago and ended yesterday.
	token, sess := auth.NewSession(u.ID, time.Now().Add(-2*auth.SessionTTL))
	if err := s.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Lookup(ctx, auth.HashToken(token)); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatalf("expired session: err = %v, want auth.ErrSessionNotFound", err)
	}
}

func TestSessionStoreDelete(t *testing.T) {
	_, u, s := newSessionFixture(t)
	ctx := context.Background()

	token, sess := auth.NewSession(u.ID, time.Now())
	if err := s.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	hash := auth.HashToken(token)
	if err := s.Delete(ctx, hash); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Lookup(ctx, hash); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatalf("after Delete: err = %v, want auth.ErrSessionNotFound", err)
	}
	if err := s.Delete(ctx, hash); err != nil {
		t.Fatalf("second Delete: %v, want nil", err)
	}
}

func TestDeletingUserEndsSessions(t *testing.T) {
	db, u, s := newSessionFixture(t)
	ctx := context.Background()

	token, sess := auth.NewSession(u.ID, time.Now())
	if err := s.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM users WHERE id = $1", u.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if _, err := s.Lookup(ctx, auth.HashToken(token)); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatalf("session outlived its user: err = %v", err)
	}
}

func TestSessionsTableConstraints(t *testing.T) {
	db, u, _ := newSessionFixture(t)
	ctx := context.Background()
	const insert = `INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES ($1, $2, $3, $4)`
	now := time.Now().UTC()
	hash := auth.HashToken("x")

	// SQLSTATE 23514 = check_violation, 23503 = foreign_key_violation.
	tests := []struct {
		name     string
		hash     []byte
		userID   string
		expires  time.Time
		wantCode string
	}{
		{"hash not 32 bytes", []byte("raw-token"), u.ID, now.Add(time.Hour), "23514"},
		{"no such user", hash, "ffffffffffffffffffffffffffffffff", now.Add(time.Hour), "23503"},
		{"expires before it starts", hash, u.ID, now.Add(-time.Hour), "23514"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, insert, tc.hash, tc.userID, now, tc.expires)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != tc.wantCode {
				t.Fatalf("err = %v, want SQLSTATE %s", err, tc.wantCode)
			}
		})
	}
}

func TestUserStoreUpdatePasswordHash(t *testing.T) {
	users := newTestUserStore(t)
	ctx := context.Background()
	u := mustNewUser(t, "dan@example.com")
	if err := users.Create(ctx, u); err != nil {
		t.Fatal(err)
	}

	newHash, err := testHasher.Hash("a brand new password")
	if err != nil {
		t.Fatal(err)
	}
	if err := users.UpdatePasswordHash(ctx, u.ID, newHash); err != nil {
		t.Fatalf("UpdatePasswordHash: %v", err)
	}
	got, err := users.GetByEmail(ctx, u.Email)
	if err != nil {
		t.Fatal(err)
	}
	if got.PasswordHash != newHash {
		t.Error("password hash was not updated")
	}
	if err := users.UpdatePasswordHash(ctx, "ffffffffffffffffffffffffffffffff", newHash); !errors.Is(err, auth.ErrUserNotFound) {
		t.Errorf("unknown user: err = %v, want auth.ErrUserNotFound", err)
	}
}
