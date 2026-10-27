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

// sessionsOf creates n sessions for userID that started at start.
func sessionsOf(t *testing.T, s *SessionStore, userID string, n int, start time.Time) []string {
	t.Helper()
	tokens := make([]string, n)
	for i := range n {
		token, sess := auth.NewSession(userID, start)
		if err := s.Create(context.Background(), sess); err != nil {
			t.Fatalf("create session: %v", err)
		}
		tokens[i] = token
	}
	return tokens
}

func countSessions(t *testing.T, db *sql.DB, userID string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), "SELECT count(*) FROM sessions WHERE user_id = $1", userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// newTwoUserFixture adds a second user to newSessionFixture's database.
func newTwoUserFixture(t *testing.T) (*sql.DB, *auth.User, *auth.User, *SessionStore) {
	t.Helper()
	db, dan, s := newSessionFixture(t)
	eve := mustNewUser(t, "eve@example.com")
	if err := NewUserStore(db).Create(context.Background(), eve); err != nil {
		t.Fatal(err)
	}
	return db, dan, eve, s
}

func TestSessionStoreDeleteExpired(t *testing.T) {
	db, u, s := newSessionFixture(t)
	ctx := context.Background()

	sessionsOf(t, s, u.ID, 3, time.Now().Add(-2*auth.SessionTTL)) // ended yesterday
	live := sessionsOf(t, s, u.ID, 2, time.Now())

	n, err := s.DeleteExpired(ctx)
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	if n != 3 {
		t.Errorf("deleted %d sessions, want the 3 expired ones", n)
	}
	if got := countSessions(t, db, u.ID); got != 2 {
		t.Errorf("%d sessions left, want the 2 live ones", got)
	}
	if _, err := s.Lookup(ctx, auth.HashToken(live[0])); err != nil {
		t.Errorf("a live session stopped working: %v", err)
	}
}

func TestSessionStoreDeleteAllForUser(t *testing.T) {
	db, dan, eve, s := newTwoUserFixture(t)

	sessionsOf(t, s, dan.ID, 3, time.Now())
	sessionsOf(t, s, eve.ID, 1, time.Now())

	n, err := s.DeleteAllForUser(context.Background(), dan.ID)
	if err != nil {
		t.Fatalf("DeleteAllForUser: %v", err)
	}
	if n != 3 || countSessions(t, db, dan.ID) != 0 {
		t.Errorf("ended %d of dan's sessions, %d left; want 3 and 0", n, countSessions(t, db, dan.ID))
	}
	if countSessions(t, db, eve.ID) != 1 {
		t.Error("ending dan's sessions also ended eve's")
	}
}

func TestUserStoreChangePassword(t *testing.T) {
	db, dan, eve, s := newTwoUserFixture(t)
	users := NewUserStore(db)
	ctx := context.Background()

	tokens := sessionsOf(t, s, dan.ID, 3, time.Now()) // laptop, phone, stolen tablet
	sessionsOf(t, s, eve.ID, 1, time.Now())
	current := auth.HashToken(tokens[0])

	newHash, err := testHasher.Hash("a brand new password")
	if err != nil {
		t.Fatal(err)
	}
	ended, err := users.ChangePassword(ctx, dan.ID, newHash, current)
	if err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if ended != 2 {
		t.Errorf("ended %d other sessions, want 2", ended)
	}

	if _, err := s.Lookup(ctx, current); err != nil {
		t.Errorf("the session that changed the password was ended: %v", err)
	}
	for _, other := range tokens[1:] {
		if _, err := s.Lookup(ctx, auth.HashToken(other)); !errors.Is(err, auth.ErrSessionNotFound) {
			t.Errorf("another session survived the password change: err = %v", err)
		}
	}
	if countSessions(t, db, eve.ID) != 1 {
		t.Error("dan's password change ended eve's session")
	}
	got, err := users.GetByEmail(ctx, dan.Email)
	if err != nil {
		t.Fatal(err)
	}
	if got.PasswordHash != newHash {
		t.Error("password hash was not changed")
	}

	if _, err := users.ChangePassword(ctx, "ffffffffffffffffffffffffffffffff", newHash, current); !errors.Is(err, auth.ErrUserNotFound) {
		t.Errorf("unknown user: err = %v, want auth.ErrUserNotFound", err)
	}
}

// If ending the other sessions fails, the password must not change either.
func TestUserStoreChangePasswordIsAllOrNothing(t *testing.T) {
	db, dan, _, _ := newTwoUserFixture(t)
	users := NewUserStore(db)
	ctx := context.Background()

	newHash, err := testHasher.Hash("a brand new password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := users.ChangePassword(ctx, dan.ID, newHash, []byte{}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// Renaming the column makes the DELETE fail after the UPDATE has run.
	_, err = db.ExecContext(ctx, `ALTER TABLE sessions RENAME COLUMN token_hash TO token_hash_gone`)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := testHasher.Hash("an even newer password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := users.ChangePassword(ctx, dan.ID, newer, []byte{}); err == nil {
		t.Fatal("ChangePassword succeeded with a broken sessions table")
	}

	got, err := users.GetByEmail(ctx, dan.Email)
	if err != nil {
		t.Fatal(err)
	}
	if got.PasswordHash != newHash {
		t.Error("the password changed although ending the sessions failed")
	}
}
