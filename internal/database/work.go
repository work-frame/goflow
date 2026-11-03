package database

import (
	"context"
	"database/sql"
	"fmt"

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

// Claim marks a pending or queued job processing and reports whether this
// caller won it. Only the winner may run the job.
//
// Two workers claiming the same job (a duplicate ID in the queue) can't both
// win, even in different processes: PostgreSQL locks the row for the first
// UPDATE, the second waits for that commit, then re-checks the WHERE clause,
// finds 'processing', and changes 0 rows. A Go mutex couldn't give this
// guarantee; it only covers goroutines in one process.
//
// Finished, failed, deleted and already-processing jobs report false with no
// error: there is nothing for this caller to do.
func (s *WorkStore) Claim(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status = 'processing', updated_at = GREATEST(now(), created_at)
		 WHERE id = $1 AND status IN ('pending', 'queued')`, id)
	if err != nil {
		return false, fmt.Errorf("database: claim job %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("database: claim job %s: %w", id, err)
	}
	return n == 1, nil
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

// Fail marks a processing job failed. Retries come in Phase 7.
func (s *WorkStore) Fail(ctx context.Context, id string) error {
	return s.finish(ctx, id, job.StatusFailed)
}

func (s *WorkStore) finish(ctx context.Context, id string, status job.Status) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status = $2, updated_at = GREATEST(now(), created_at)
		 WHERE id = $1 AND status = 'processing'`, id, string(status)); err != nil {
		return fmt.Errorf("database: mark job %s %s: %w", id, status, err)
	}
	return nil
}
