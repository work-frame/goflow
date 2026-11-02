package database

import (
	"context"
	"database/sql"
	"fmt"
)

// OutboxStore reads and clears job_outbox, the list of jobs whose IDs still
// have to reach the queue. JobStore.Add fills it.
type OutboxStore struct {
	db *sql.DB
}

// NewOutboxStore returns an OutboxStore that uses db.
func NewOutboxStore(db *sql.DB) *OutboxStore {
	return &OutboxStore{db: db}
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
	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET status = 'queued', updated_at = GREATEST(now(), created_at) WHERE id = ANY($1) AND status = 'pending'`,
		jobIDs); err != nil {
		return 0, fmt.Errorf("database: relay: mark queued: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("database: relay: commit: %w", err)
	}
	return len(jobIDs), nil
}

// Waiting reports how many jobs are waiting in the outbox.
func (s *OutboxStore) Waiting(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM job_outbox`).Scan(&n); err != nil {
		return 0, fmt.Errorf("database: outbox count: %w", err)
	}
	return n, nil
}
