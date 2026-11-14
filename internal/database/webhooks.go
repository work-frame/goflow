package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/work-frame/goflow/internal/job"
	"github.com/work-frame/goflow/internal/secrets"
)

// SecretOverlap is how long a rotated-out signing secret keeps signing, so
// receivers can switch without dropping a delivery (Task 65, decision K5).
const SecretOverlap = 24 * time.Hour

// ErrNoSecret means the user has no webhook signing secret yet.
var ErrNoSecret = errors.New("database: no webhook signing secret")

// SecretStore keeps each user's webhook signing secret, encrypted. Callers
// see plaintext only when they create one (shown to the user once) and when
// a worker signs a delivery.
type SecretStore struct {
	db   *sql.DB
	keys *secrets.Keyring
}

// NewSecretStore returns a SecretStore that encrypts with keys.
func NewSecretStore(db *sql.DB, keys *secrets.Keyring) *SecretStore {
	return &SecretStore{db: db, keys: keys}
}

// secretContext binds a ciphertext to its owner: copied onto another
// user's row, it doesn't decrypt (FAILURES.md, case 60).
func secretContext(userID string) []byte {
	return []byte("goflow webhook secret v1|" + userID)
}

// Rotate creates the user's signing secret, or replaces it. The secret it
// replaces becomes the previous one and keeps signing for SecretOverlap;
// one that was already previous is dropped. It returns the new secret, the
// only time it is ever shown, and when the previous one stops signing (zero
// if there is none).
func (s *SecretStore) Rotate(ctx context.Context, userID string) (secret string, previousUntil time.Time, err error) {
	secret = secrets.NewSigningSecret()
	sealed := s.keys.Seal([]byte(secret), secretContext(userID))
	var rotated time.Time
	var hadPrevious bool
	err = s.db.QueryRowContext(ctx,
		`INSERT INTO webhook_secrets (user_id, key_id, nonce, ciphertext, rotated_at)
		 VALUES ($1, $2, $3, $4, now())
		 ON CONFLICT (user_id) DO UPDATE SET
		     prev_key_id = webhook_secrets.key_id, prev_nonce = webhook_secrets.nonce,
		     prev_ciphertext = webhook_secrets.ciphertext,
		     key_id = EXCLUDED.key_id, nonce = EXCLUDED.nonce, ciphertext = EXCLUDED.ciphertext,
		     rotated_at = EXCLUDED.rotated_at
		 RETURNING rotated_at, prev_key_id IS NOT NULL`,
		userID, sealed.KeyID, sealed.Nonce, sealed.Ciphertext).Scan(&rotated, &hadPrevious)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("database: rotate webhook secret: %w", err)
	}
	if hadPrevious {
		previousUntil = rotated.Add(SecretOverlap).UTC()
	}
	return secret, previousUntil, nil
}

// Has reports whether the user has a signing secret.
func (s *SecretStore) Has(ctx context.Context, userID string) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM webhook_secrets WHERE user_id = $1)`, userID).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("database: webhook secret exists: %w", err)
	}
	return ok, nil
}

// Signing returns the secrets a delivery is signed with right now: the
// current one, then the previous one while its overlap lasts. Whether the
// overlap lasts is decided by the database's clock, like leases.
func (s *SecretStore) Signing(ctx context.Context, userID string) ([]string, error) {
	var cur, prev secrets.Sealed
	var prevKey sql.NullString
	var inOverlap bool
	err := s.db.QueryRowContext(ctx,
		`SELECT key_id, nonce, ciphertext, prev_key_id, prev_nonce, prev_ciphertext,
		        rotated_at > now() - make_interval(secs => $2)
		 FROM webhook_secrets WHERE user_id = $1`, userID, SecretOverlap.Seconds()).
		Scan(&cur.KeyID, &cur.Nonce, &cur.Ciphertext, &prevKey, &prev.Nonce, &prev.Ciphertext, &inOverlap)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoSecret
	}
	if err != nil {
		return nil, fmt.Errorf("database: webhook secret: %w", err)
	}
	p, err := s.keys.Open(cur, secretContext(userID))
	if err != nil {
		return nil, fmt.Errorf("database: webhook secret: %w", err)
	}
	out := []string{string(p)}
	if prevKey.Valid && inOverlap {
		prev.KeyID = prevKey.String
		p, err := s.keys.Open(prev, secretContext(userID))
		if err != nil {
			return nil, fmt.Errorf("database: previous webhook secret: %w", err)
		}
		out = append(out, string(p))
	}
	return out, nil
}

// MissingKeysError lists the keys some stored secrets need that the
// Keyring doesn't have: key ID to number of secrets.
type MissingKeysError map[string]int

func (e MissingKeysError) Error() string {
	return fmt.Sprintf("webhook secrets are encrypted with keys that aren't loaded (key ID: secrets): %v; put those keys back in GOFLOW_SECRETS_OLD_KEYS (DEPLOYMENT.md, rotating the secrets key)", map[string]int(e))
}

// Reencrypt moves every stored secret to the current key, and returns how
// many it changed. Before changing anything it checks that every key in
// use is loaded: if one isn't, it changes nothing and returns a
// MissingKeysError, so the API refuses to start instead of losing secrets
// (Task 65, decision K4). Safe to run in several processes at once: each
// row is re-read under a lock.
func (s *SecretStore) Reencrypt(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT k, count(*) FROM (
		     SELECT key_id AS k FROM webhook_secrets
		     UNION ALL SELECT prev_key_id FROM webhook_secrets WHERE prev_key_id IS NOT NULL
		 ) used GROUP BY k`)
	if err != nil {
		return 0, fmt.Errorf("database: webhook secret keys: %w", err)
	}
	missing := MissingKeysError{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			rows.Close()
			return 0, fmt.Errorf("database: webhook secret keys: %w", err)
		}
		if !s.keys.Has(id) {
			missing[id] = n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("database: webhook secret keys: %w", err)
	}
	if len(missing) > 0 {
		return 0, missing
	}

	cur := s.keys.CurrentID()
	ids, err := s.db.QueryContext(ctx,
		`SELECT user_id FROM webhook_secrets WHERE key_id <> $1 OR prev_key_id <> $1`, cur)
	if err != nil {
		return 0, fmt.Errorf("database: webhook secrets to re-encrypt: %w", err)
	}
	var users []string
	for ids.Next() {
		var u string
		if err := ids.Scan(&u); err != nil {
			ids.Close()
			return 0, err
		}
		users = append(users, u)
	}
	ids.Close()
	if err := ids.Err(); err != nil {
		return 0, err
	}

	changed := 0
	for _, u := range users {
		ok, err := s.reencryptOne(ctx, u)
		if err != nil {
			return changed, err
		}
		if ok {
			changed++
		}
	}
	return changed, nil
}

func (s *SecretStore) reencryptOne(ctx context.Context, userID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var cur, prev secrets.Sealed
	var prevKey sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT key_id, nonce, ciphertext, prev_key_id, prev_nonce, prev_ciphertext
		 FROM webhook_secrets WHERE user_id = $1 FOR UPDATE`, userID).
		Scan(&cur.KeyID, &cur.Nonce, &cur.Ciphertext, &prevKey, &prev.Nonce, &prev.Ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // deleted meanwhile
	}
	if err != nil {
		return false, err
	}
	ctxData := secretContext(userID)
	moved := false
	if cur.KeyID != s.keys.CurrentID() {
		p, err := s.keys.Open(cur, ctxData)
		if err != nil {
			return false, fmt.Errorf("database: re-encrypt a webhook secret: %w", err)
		}
		cur = s.keys.Seal(p, ctxData)
		moved = true
	}
	var prevArgs [3]any
	if prevKey.Valid {
		prev.KeyID = prevKey.String
		if prev.KeyID != s.keys.CurrentID() {
			p, err := s.keys.Open(prev, ctxData)
			if err != nil {
				return false, fmt.Errorf("database: re-encrypt a previous webhook secret: %w", err)
			}
			prev = s.keys.Seal(p, ctxData)
			moved = true
		}
		prevArgs = [3]any{prev.KeyID, prev.Nonce, prev.Ciphertext}
	}
	if !moved {
		return false, nil // another process got here first
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE webhook_secrets SET key_id = $2, nonce = $3, ciphertext = $4,
		        prev_key_id = $5, prev_nonce = $6, prev_ciphertext = $7
		 WHERE user_id = $1`,
		userID, cur.KeyID, cur.Nonce, cur.Ciphertext, prevArgs[0], prevArgs[1], prevArgs[2]); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// AttemptStore records and lists webhook delivery attempts.
type AttemptStore struct{ db *sql.DB }

// NewAttemptStore returns an AttemptStore on db.
func NewAttemptStore(db *sql.DB) *AttemptStore { return &AttemptStore{db: db} }

// Record stores one attempt of job jobID. A job deleted meanwhile is not an
// error: its attempts would be deleted with it anyway.
func (s *AttemptStore) Record(ctx context.Context, jobID string, a job.Attempt) error {
	var code any
	if a.StatusCode != 0 {
		code = a.StatusCode
	}
	var msg any
	if a.Error != "" {
		msg = truncateRunes(a.Error, job.MaxAttemptError)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO webhook_attempts (job_id, attempt, started_at, duration_ms, status_code, outcome, error)
		 SELECT $1, $2, $3, $4, $5, $6, $7 WHERE EXISTS (SELECT 1 FROM jobs WHERE id = $1)`,
		jobID, a.Attempt, a.StartedAt, a.DurationMS, code, a.Outcome, msg)
	if err != nil {
		return fmt.Errorf("database: record webhook attempt for job %s: %w", jobID, err)
	}
	return nil
}

// List returns the attempts of userID's job jobID, oldest first, or
// job.ErrNotFound if userID has no such job.
func (s *AttemptStore) List(ctx context.Context, userID, jobID string) ([]job.Attempt, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM jobs WHERE id = $1 AND user_id = $2)`, jobID, userID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("database: webhook attempts: %w", err)
	}
	if !exists {
		return nil, job.ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT a.attempt, a.started_at, a.duration_ms, coalesce(a.status_code, 0), a.outcome, coalesce(a.error, '')
		 FROM webhook_attempts a JOIN jobs j ON j.id = a.job_id
		 WHERE a.job_id = $1 AND j.user_id = $2 ORDER BY a.id`, jobID, userID)
	if err != nil {
		return nil, fmt.Errorf("database: webhook attempts: %w", err)
	}
	defer rows.Close()
	out := []job.Attempt{}
	for rows.Next() {
		var a job.Attempt
		if err := rows.Scan(&a.Attempt, &a.StartedAt, &a.DurationMS, &a.StatusCode, &a.Outcome, &a.Error); err != nil {
			return nil, err
		}
		a.StartedAt = a.StartedAt.UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
