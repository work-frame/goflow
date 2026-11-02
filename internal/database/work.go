package database

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/work-frame/goflow/internal/job"
)

// WorkStore is what a worker needs from the database: claim a job, then
// record how it ended. Unlike JobStore it isn't scoped to a user. Workers
// run every user's jobs, and only ever by an ID taken from the queue.
type WorkStore struct {
	db *sql.DB
}

// NewWorkStore returns a WorkStore that uses db.
func NewWorkStore(db *sql.DB) *WorkStore {
	return &WorkStore{db: db}
}

// Claim marks the job processing and reports whether the worker should run
// it. It reports false, with no error, when there is nothing to do: the job
// was deleted, or it already completed or failed. That is what makes a
// duplicate ID harmless: the second copy finds the job finished and skips it.
//
// A job already processing can be claimed again. With one worker that only
// happens when the worker died mid-job, and running it again is the point.
// Several workers at once will need more than this (Phase 6).
func (s *WorkStore) Claim(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status = 'processing', updated_at = GREATEST(now(), created_at)
		 WHERE id = $1 AND status IN ('pending', 'queued', 'processing')`, id)
	if err != nil {
		return false, fmt.Errorf("database: claim job %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("database: claim job %s: %w", id, err)
	}
	return n == 1, nil
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
