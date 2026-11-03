package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// OutboxStore reads and clears job_outbox, the list of jobs whose IDs still
// have to reach the queue. JobStore.Add fills it.
type OutboxStore struct {
	db     *sql.DB
	notify Notifier // nil: nobody is told about changes
}

// NewOutboxStore returns an OutboxStore that uses db.
func NewOutboxStore(db *sql.DB) *OutboxStore {
	return &OutboxStore{db: db}
}

// WithNotifier returns a copy of s that tells n about every job Relay marks
// queued, after the change is committed.
func (s *OutboxStore) WithNotifier(n Notifier) *OutboxStore {
	c := *s
	c.notify = n
	return &c
}

// Relay takes up to limit outbox rows, oldest first, and calls send with
// their job IDs. If send succeeds, the rows are deleted and the jobs marked
// queued, in the same transaction. If send fails, nothing changes and the
// rows wait for the next try. It returns how many IDs were sent.
//
// Delivery is at least once, never at most once: if send succeeds but the
// commit then fails (the database goes away, the process dies), the rows
// are still there and the same IDs are sent again later. Workers must
// therefore cope with seeing a job twice.
//
// FOR UPDATE SKIP LOCKED lets several API instances relay at the same time:
// each locks the rows it took, and the others skip them instead of waiting.
func (s *OutboxStore) Relay(ctx context.Context, limit int, send func(ctx context.Context, jobIDs []string) error) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("database: relay: begin: %w", err)
	}
	defer tx.Rollback() // does nothing once Commit has succeeded

	rows, err := tx.QueryContext(ctx,
		`SELECT id, job_id FROM job_outbox ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return 0, fmt.Errorf("database: relay: select: %w", err)
	}
	var rowIDs []int64
	var jobIDs []string
	for rows.Next() {
		var rowID int64
		var jobID string
		if err := rows.Scan(&rowID, &jobID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("database: relay: scan: %w", err)
		}
		rowIDs = append(rowIDs, rowID)
		jobIDs = append(jobIDs, jobID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("database: relay: rows: %w", err)
	}
	if len(jobIDs) == 0 {
		return 0, nil
	}

	// The rows stay locked while send talks to Redis, so no other instance
	// sends the same IDs at the same time.
	if err := send(ctx, jobIDs); err != nil {
		return 0, err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM job_outbox WHERE id = ANY($1)`, rowIDs); err != nil {
		return 0, fmt.Errorf("database: relay: delete: %w", err)
	}
	// Only pending jobs move to queued; a job a worker already picked up
	// (Phase 6) must not go back. created_at comes from the API's clock and
	// now() from the database's; GREATEST keeps updated_at >= created_at
	// (a CHECK constraint) even if the two clocks disagree.
	rows, err = tx.QueryContext(ctx,
		`UPDATE jobs SET status = 'queued', updated_at = GREATEST(now(), created_at) WHERE id = ANY($1) AND status = 'pending'
		 RETURNING `+jobColumns, jobIDs)
	if err != nil {
		return 0, fmt.Errorf("database: relay: mark queued: %w", err)
	}
	queued, err := scanJobs(rows)
	if err != nil {
		return 0, fmt.Errorf("database: relay: mark queued: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("database: relay: commit: %w", err)
	}
	notifyChanged(ctx, s.notify, queued...)
	return len(jobIDs), nil
}

// RequeueStale finds up to limit jobs that have been 'queued' longer than
// olderThan, oldest first, and calls send with their IDs. If send succeeds,
// their updated_at is moved to now, so the next run doesn't send them again
// unless they stay stuck for another olderThan. It returns how many it sent.
//
// It also re-sends 'retrying' jobs whose next_attempt_at passed more than
// olderThan ago, straight to the pending list, and moves next_attempt_at to
// now: their ID should have left Redis's delayed set long ago.
//
// A job is 'queued' once the relay has put its ID in Redis. Staying queued
// (or overdue while retrying) for long means no worker has claimed it:
// either the queue is backed up, or Redis lost the ID (a free Key Value
// restart empties every list and the delayed set). Sending it again costs at
// most a duplicate, which Claim makes harmless.
func (s *OutboxStore) RequeueStale(ctx context.Context, olderThan time.Duration, limit int, send func(ctx context.Context, jobIDs []string) error) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("database: requeue stale: begin: %w", err)
	}
	defer tx.Rollback() // does nothing once Commit has succeeded

	// make_interval takes seconds as a double; olderThan never comes from users.
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM jobs
		 WHERE (status = 'queued' AND updated_at < now() - make_interval(secs => $1))
		    OR (status = 'retrying' AND next_attempt_at < now() - make_interval(secs => $1))
		 ORDER BY updated_at, id LIMIT $2 FOR UPDATE SKIP LOCKED`,
		olderThan.Seconds(), limit)
	if err != nil {
		return 0, fmt.Errorf("database: requeue stale: select: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("database: requeue stale: scan: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("database: requeue stale: rows: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}

	if err := send(ctx, ids); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET updated_at = GREATEST(now(), created_at),
		        next_attempt_at = CASE WHEN status = 'retrying' THEN now() ELSE next_attempt_at END
		 WHERE id = ANY($1) AND status IN ('queued', 'retrying')`, ids); err != nil {
		return 0, fmt.Errorf("database: requeue stale: touch: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("database: requeue stale: commit: %w", err)
	}
	return len(ids), nil
}

// Waiting reports how many jobs are waiting in the outbox.
func (s *OutboxStore) Waiting(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM job_outbox`).Scan(&n); err != nil {
		return 0, fmt.Errorf("database: outbox count: %w", err)
	}
	return n, nil
}
