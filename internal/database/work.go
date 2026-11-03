package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/work-frame/goflow/internal/job"
)

// WorkStore is what workers need from the database: claim a job, record how
// it ended, and recover jobs a stopped process left behind. Unlike JobStore
// it isn't scoped to a user. Workers run every user's jobs, and only ever by
// an ID taken from the queue.
type WorkStore struct {
	db *sql.DB
}

// NewWorkStore returns a WorkStore that uses db.
func NewWorkStore(db *sql.DB) *WorkStore {
	return &WorkStore{db: db}
}

// Claim marks a pending, queued or retrying job processing, counts the
// attempt, and returns its number (1 for the first run), the job's
// max_attempts and its title. attempt 0 means this caller didn't win the job and must not
// run it.
//
// Two workers claiming the same job (a duplicate ID in the queue) can't both
// win, even in different processes: PostgreSQL locks the row for the first
// UPDATE, the second waits for that commit, then re-checks the WHERE clause,
// finds 'processing', and changes 0 rows. A Go mutex couldn't give this
// guarantee; it only covers goroutines in one process.
//
// A retrying job is claimable even if its next_attempt_at hasn't quite come:
// Redis decides when its ID is delivered, and the two clocks may differ by a
// second. Refusing it here would drop the ID for good.
//
// Finished, dead, deleted and already-processing jobs return attempt 0 with
// no error: there is nothing for this caller to do.
func (s *WorkStore) Claim(ctx context.Context, id string) (attempt, maxAttempts int, title string, err error) {
	err = s.db.QueryRowContext(ctx,
		`UPDATE jobs SET status = 'processing', attempts = attempts + 1, next_attempt_at = NULL,
		        updated_at = GREATEST(now(), created_at)
		 WHERE id = $1 AND status IN ('pending', 'queued', 'retrying')
		 RETURNING attempts, max_attempts, title`, id).Scan(&attempt, &maxAttempts, &title)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, "", nil
	}
	if err != nil {
		return 0, 0, "", fmt.Errorf("database: claim job %s: %w", id, err)
	}
	return attempt, maxAttempts, title, nil
}

// ResetProcessing puts every processing job back to queued and returns how
// many it changed. A worker process calls it once at startup, before any of
// its workers claim anything: jobs still marked processing then belong to a
// process that stopped mid-job, and resetting them makes them claimable again.
//
// SAFE ONLY WITH ONE WORKER PROCESS. It can't tell a dead process's jobs from
// a live one's: a second process starting while the first is running would
// reset jobs that are still being worked on, and they would run twice at the
// same time. Several processes need leases instead (who holds a job, until
// when), so only expired claims are taken over. See ARCHITECTURE.md.
func (s *WorkStore) ResetProcessing(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status = 'queued', updated_at = GREATEST(now(), created_at) WHERE status = 'processing'`)
	if err != nil {
		return 0, fmt.Errorf("database: reset processing jobs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("database: reset processing jobs: %w", err)
	}
	return n, nil
}

// Complete marks a processing job completed.
func (s *WorkStore) Complete(ctx context.Context, id string) error {
	return s.finish(ctx, id, job.StatusCompleted)
}

// Retry records a failed attempt that will run again: the job becomes
// 'retrying', due after delay (counted on the database's clock), with its
// error kept for operators. It returns when the retry is due, for the
// caller to schedule in Redis, which it must do only after this has
// committed. The job must be 'processing'.
func (s *WorkStore) Retry(ctx context.Context, id string, delay time.Duration, lastError string) (time.Time, error) {
	var due time.Time
	err := s.db.QueryRowContext(ctx,
		`UPDATE jobs SET status = 'retrying', last_error = $3,
		        next_attempt_at = now() + make_interval(secs => $2),
		        updated_at = GREATEST(now(), created_at)
		 WHERE id = $1 AND status = 'processing'
		 RETURNING next_attempt_at`, id, delay.Seconds(), truncateError(lastError)).Scan(&due)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, fmt.Errorf("database: retry job %s: %w", id, ErrNotProcessing)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("database: retry job %s: %w", id, err)
	}
	return due.UTC(), nil
}

// Bury marks a processing job dead: a permanent error, or out of attempts.
func (s *WorkStore) Bury(ctx context.Context, id string, lastError string) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status = 'dead', last_error = $2, next_attempt_at = NULL,
		        updated_at = GREATEST(now(), created_at)
		 WHERE id = $1 AND status = 'processing'`, id, truncateError(lastError)); err != nil {
		return fmt.Errorf("database: bury job %s: %w", id, err)
	}
	return nil
}

// ErrNotProcessing means a job's outcome couldn't be recorded because it
// wasn't 'processing' any more (deleted, or reset by a restart).
var ErrNotProcessing = errors.New("job is not processing")

// maxErrorLength matches the last_error CHECK constraint.
const maxErrorLength = 1000

// truncateError keeps an error message within maxErrorLength characters.
func truncateError(msg string) string {
	r := []rune(msg)
	if len(r) <= maxErrorLength {
		return msg
	}
	return string(r[:maxErrorLength-3]) + "..."
}

func (s *WorkStore) finish(ctx context.Context, id string, status job.Status) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status = $2, updated_at = GREATEST(now(), created_at)
		 WHERE id = $1 AND status = 'processing'`, id, string(status)); err != nil {
		return fmt.Errorf("database: mark job %s %s: %w", id, status, err)
	}
	return nil
}
