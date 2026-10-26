package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/work-frame/goflow/internal/auth"
)

// usersEmailKey is the name PostgreSQL gave the UNIQUE constraint on users.email
// (<table>_<column>_key). It tells a taken email apart from any other duplicate.
const usersEmailKey = "users_email_key"

// UserStore keeps accounts in PostgreSQL. Callers only see auth package errors.
// Error messages name users by ID, never by email: emails are personal data
// and these errors end up in logs.
type UserStore struct {
	db *sql.DB
}

// NewUserStore returns a UserStore that uses db.
func NewUserStore(db *sql.DB) *UserStore {
	return &UserStore{db: db}
}

// Create inserts u. It returns auth.ErrEmailTaken if the email is registered.
func (s *UserStore) Create(ctx context.Context, u *auth.User) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id, email, password_hash, created_at) VALUES ($1, $2, $3, $4)`,
		u.ID, u.Email, u.PasswordHash, u.CreatedAt,
	)

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == usersEmailKey {
		return auth.ErrEmailTaken
	}
	if err != nil {
		return fmt.Errorf("database: create user %s: %w", u.ID, err)
	}
	return nil
}

// GetByEmail returns the user with this email, or auth.ErrUserNotFound.
// email must already be normalized (auth.NormalizeEmail).
func (s *UserStore) GetByEmail(ctx context.Context, email string) (*auth.User, error) {
	var u auth.User
	err := s.db.QueryRowContext(ctx,
		`SELECT id, email, password_hash, created_at FROM users WHERE email = $1`, email,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, auth.ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("database: get user by email: %w", err)
	}
	u.CreatedAt = u.CreatedAt.UTC()
	return &u, nil
}
