package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/work-frame/goflow/internal/auth"
)

// APIKeyStore keeps API keys in PostgreSQL (Task 86). Like SessionStore it
// only ever sees a key's hash; the secret never reaches the database.
//
// Whether a key works is decided by PostgreSQL's clock on every request:
// revoked_at IS NULL and expires_at > now(). Nothing is cached, so a
// revoked key fails on its very next request.
type APIKeyStore struct {
	db *sql.DB
}

// NewAPIKeyStore returns an APIKeyStore that uses db.
func NewAPIKeyStore(db *sql.DB) *APIKeyStore {
	return &APIKeyStore{db: db}
}

// keyStatus is a key's status on the database's clock.
const keyStatus = `CASE WHEN revoked_at IS NOT NULL THEN 'revoked' WHEN expires_at <= now() THEN 'expired' ELSE 'active' END`

const keyColumns = `id, user_id, name, hint, array_to_string(scopes, ','), created_at, expires_at, last_used_at, revoked_at, ` + keyStatus

// Create stores k, unless its owner already has auth.MaxActiveKeys active
// keys (auth.ErrTooManyKeys). The owner's users row is locked first, so
// two creates at once can't both see 9 and make 11: the second waits for
// the first to commit.
//
// The lock and the count are two statements on purpose. In one statement
// ("SELECT (SELECT count(*) ...) FROM users ... FOR UPDATE"), a request
// that waited for the lock re-reads the users row but NOT the subquery:
// the count still comes from before it waited. 20 creates at once made 17
// keys that way (found while building Task 86). Under READ COMMITTED each
// statement takes a fresh snapshot, so a count run after the lock is
// granted sees every key committed before it.
func (s *APIKeyStore) Create(ctx context.Context, k *auth.APIKey) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("database: create key for user %s: %w", k.UserID, err)
	}
	defer tx.Rollback() // a no-op after Commit

	var locked bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM users WHERE id = $1 FOR UPDATE`, k.UserID).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.ErrUserNotFound
	}
	if err != nil {
		return fmt.Errorf("database: create key for user %s: %w", k.UserID, err)
	}
	var active int
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) FROM api_keys WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > now()`,
		k.UserID).Scan(&active); err != nil {
		return fmt.Errorf("database: create key for user %s: %w", k.UserID, err)
	}
	if active >= auth.MaxActiveKeys {
		return auth.ErrTooManyKeys
	}

	var status string
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO api_keys (id, user_id, name, key_hash, hint, scopes, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING `+keyStatus,
		k.ID, k.UserID, k.Name, k.KeyHash, k.Hint, scopeStrings(k.Scopes), k.CreatedAt, k.ExpiresAt,
	).Scan(&status); err != nil {
		return fmt.Errorf("database: create key for user %s: %w", k.UserID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("database: create key for user %s: %w", k.UserID, err)
	}
	k.Status = status
	return nil
}

// Lookup returns the key with this hash and its owner, if the key is
// neither revoked nor expired; otherwise auth.ErrKeyNotFound, the same for
// every reason. stale reports that last_used_at is unset or more than a
// minute old, on the database's clock: the caller then calls Touch.
func (s *APIKeyStore) Lookup(ctx context.Context, keyHash []byte) (u *auth.User, k *auth.APIKey, stale bool, err error) {
	u, k = &auth.User{}, &auth.APIKey{}
	var scopes string
	var used, revoked sql.NullTime
	err = s.db.QueryRowContext(ctx, `
		SELECT u.id, u.email, u.password_hash, u.created_at,
		       k.id, k.name, k.hint, array_to_string(k.scopes, ','), k.created_at, k.expires_at, k.last_used_at, k.revoked_at,
		       k.last_used_at IS NULL OR k.last_used_at < now() - interval '1 minute'
		FROM api_keys k
		JOIN users u ON u.id = k.user_id
		WHERE k.key_hash = $1 AND k.revoked_at IS NULL AND k.expires_at > now()`,
		keyHash,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt,
		&k.ID, &k.Name, &k.Hint, &scopes, &k.CreatedAt, &k.ExpiresAt, &used, &revoked, &stale)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, false, auth.ErrKeyNotFound
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf("database: look up key: %w", err)
	}
	u.CreatedAt = u.CreatedAt.UTC()
	k.UserID = u.ID
	k.Scopes = toScopes(scopes)
	k.CreatedAt, k.ExpiresAt = k.CreatedAt.UTC(), k.ExpiresAt.UTC()
	k.LastUsedAt, k.RevokedAt = timePtr(used), timePtr(revoked)
	k.Status = auth.KeyActive
	return u, k, stale, nil
}

// Touch records that key id was just used, at most once a minute. The
// condition is in the UPDATE itself, not only in Go: requests that arrive
// together all queue on the row lock, and READ COMMITTED checks the
// condition again for each after the first commits, so only the first
// writes a row version; the rest match nothing and write nothing.
func (s *APIKeyStore) Touch(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE api_keys SET last_used_at = now()
		WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute')`, id); err != nil {
		return fmt.Errorf("database: touch key %s: %w", id, err)
	}
	return nil
}

// List returns every key userID has ever had, active, expired and revoked,
// newest first. No hashes: there is no column for them in keyColumns.
func (s *APIKeyStore) List(ctx context.Context, userID string) ([]*auth.APIKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+keyColumns+` FROM api_keys WHERE user_id = $1 ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("database: list keys: %w", err)
	}
	defer rows.Close()
	keys := []*auth.APIKey{}
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, fmt.Errorf("database: scan key: %w", err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("database: list keys: %w", err)
	}
	return keys, nil
}

// Revoke ends key id if userID owns it. Revoking a key that is already
// revoked is not an error; someone else's key, or none, is
// auth.ErrKeyNotFound. The row stays, as history.
func (s *APIKeyStore) Revoke(ctx context.Context, userID, id string) error {
	var found bool
	err := s.db.QueryRowContext(ctx, `
		WITH revoked AS (
			UPDATE api_keys SET revoked_at = now()
			WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL
			RETURNING id
		)
		SELECT EXISTS (SELECT 1 FROM revoked) OR EXISTS (SELECT 1 FROM api_keys WHERE id = $1 AND user_id = $2)`,
		id, userID).Scan(&found)
	if err != nil {
		return fmt.Errorf("database: revoke key %s: %w", id, err)
	}
	if !found {
		return auth.ErrKeyNotFound
	}
	return nil
}

// RevokeAll ends every active key userID has and returns how many. Expired
// keys are left as they are: already dead, and their history says so.
func (s *APIKeyStore) RevokeAll(ctx context.Context, userID string) (int64, error) {
	return revokeAllKeys(ctx, s.db, userID)
}

// execer is what *sql.DB and *sql.Tx have in common for one statement.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// revokeAllKeys is RevokeAll for either the pool or a transaction:
// ResetPassword runs it inside its own.
func revokeAllKeys(ctx context.Context, db execer, userID string) (int64, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE api_keys SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > now()`, userID)
	if err != nil {
		return 0, fmt.Errorf("database: revoke keys for user %s: %w", userID, err)
	}
	return res.RowsAffected()
}

func scanKey(s scanner) (*auth.APIKey, error) {
	var k auth.APIKey
	var scopes string
	var used, revoked sql.NullTime
	if err := s.Scan(&k.ID, &k.UserID, &k.Name, &k.Hint, &scopes, &k.CreatedAt, &k.ExpiresAt, &used, &revoked, &k.Status); err != nil {
		return nil, err
	}
	k.Scopes = toScopes(scopes)
	k.CreatedAt, k.ExpiresAt = k.CreatedAt.UTC(), k.ExpiresAt.UTC()
	k.LastUsedAt, k.RevokedAt = timePtr(used), timePtr(revoked)
	return &k, nil
}

func scopeStrings(scopes []auth.Scope) []string {
	out := make([]string, len(scopes))
	for i, s := range scopes {
		out[i] = string(s)
	}
	return out
}

// toScopes splits array_to_string(scopes, ','): database/sql can't scan a
// PostgreSQL array, and no scope contains a comma.
func toScopes(joined string) []auth.Scope {
	ss := strings.Split(joined, ",")
	out := make([]auth.Scope, len(ss))
	for i, s := range ss {
		out[i] = auth.Scope(s)
	}
	return out
}

func timePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	u := t.Time.UTC()
	return &u
}
