package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/work-frame/goflow/internal/auth"
)

// SessionStore keeps logins in PostgreSQL. It only ever sees token hashes.
type SessionStore struct {
	db *sql.DB
}

// NewSessionStore returns a SessionStore that uses db.
func NewSessionStore(db *sql.DB) *SessionStore {
	return &SessionStore{db: db}
}

// Create stores s.
func (s *SessionStore) Create(ctx context.Context, sess *auth.Session) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES ($1, $2, $3, $4)`,
		sess.TokenHash, sess.UserID, sess.CreatedAt, sess.ExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("database: create session for user %s: %w", sess.UserID, err)
	}
	return nil
}

// Lookup returns the user who owns the session with this token hash.
// An unknown or expired session gives auth.ErrSessionNotFound. Expiry is
// checked against the database clock, so every API instance agrees on it.
func (s *SessionStore) Lookup(ctx context.Context, tokenHash []byte) (*auth.User, error) {
	var u auth.User
	err := s.db.QueryRowContext(ctx, `
		SELECT u.id, u.email, u.password_hash, u.created_at
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > now()`,
		tokenHash,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, auth.ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("database: look up session: %w", err)
	}
	u.CreatedAt = u.CreatedAt.UTC()
	return &u, nil
}

// Delete ends the session with this token hash. Deleting a session that is
// already gone is not an error: the result the caller wants is the same.
func (s *SessionStore) Delete(ctx context.Context, tokenHash []byte) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = $1`, tokenHash); err != nil {
		return fmt.Errorf("database: delete session: %w", err)
	}
	return nil
}
