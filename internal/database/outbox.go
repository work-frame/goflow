package database

import (
	"cmp"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"slices"
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

// RelayClaimFor is how long a relay pass owns the outbox rows it claimed.
// It must be far longer than one send to Redis (2 s in internal/relay): a
// claim that runs out mid-send lets another pass send the same IDs, which
// is harmless but wasteful.
const RelayClaimFor = 30 * time.Second

// ErrOutboxBusy means every waiting outbox row is claimed by another relay
// pass (or by one that died and whose claim hasn't run out yet). Nothing was
// sent; try again later.
var ErrOutboxBusy = errors.New("database: relay: every waiting outbox row is claimed by another pass")

// Relay sends up to limit outbox rows, oldest first, in three steps:
//
//  1. Claim: one short statement marks free rows (never claimed, or whose
//     claim ran out) with a new token and claimed_until = now() + 30 s on
//     PostgreSQL's clock, and commits. FOR UPDATE SKIP LOCKED inside it only
//     keeps two passes from claiming the same row in the same instant.
//  2. Send: send gets the job IDs while no row is locked, so deleting a job
//     or an account never waits for Redis (Phase 17; FAILURES.md, case 66).
//  3. Finish: one transaction deletes only the rows that still carry this
//     pass's token and marks those jobs queued. A row whose job was deleted
//     meanwhile is gone; a row whose claim ran out and was taken by another
//     pass has that pass's token. Neither is touched.
//
// If send fails, the claim is released at once, so the next try can send
// the rows. It returns how many rows it finished.
//
// Delivery is at least once, never at most once: if send succeeds but the
// pass then stops (the database goes away, the process dies), the rows keep
// their claim until it runs out, and the same IDs are sent again. Workers
// therefore cope with seeing a job twice: Claim lets one of them win.
func (s *OutboxStore) Relay(ctx context.Context, limit int, send func(ctx context.Context, jobIDs []string) error) (int, error) {
	token := rand.Text() // 26 characters, 130 random bits
	rows, err := s.db.QueryContext(ctx,
		`UPDATE job_outbox SET claim_token = $1, claimed_until = now() + make_interval(secs => $2)
		 WHERE id IN (SELECT id FROM job_outbox WHERE claimed_until IS NULL OR claimed_until < now()
		              ORDER BY id LIMIT $3 FOR UPDATE SKIP LOCKED)
		 RETURNING id, job_id`, token, RelayClaimFor.Seconds(), limit)
	if err != nil {
		return 0, fmt.Errorf("database: relay: claim: %w", err)
	}
	type claimed struct {
		rowID int64
		jobID string
	}
	var batch []claimed
	for rows.Next() {
		var c claimed
		if err := rows.Scan(&c.rowID, &c.jobID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("database: relay: scan: %w", err)
		}
		batch = append(batch, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("database: relay: claim: %w", err)
	}
	if len(batch) == 0 {
		var rowsLeft bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM job_outbox)`).Scan(&rowsLeft); err != nil {
			return 0, fmt.Errorf("database: relay: count: %w", err)
		}
		if rowsLeft {
			return 0, ErrOutboxBusy
		}
		return 0, nil
	}
	// RETURNING doesn't promise an order; Redis gets the oldest first.
	slices.SortFunc(batch, func(a, b claimed) int { return cmp.Compare(a.rowID, b.rowID) })
	jobIDs := make([]string, len(batch))
	for i, c := range batch {
		jobIDs[i] = c.jobID
	}

	if err := send(ctx, jobIDs); err != nil {
		// Give the rows back so the next try can send them. The pass's ctx
		// may be what failed, so this gets a few seconds of its own.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, rerr := s.db.ExecContext(rctx,
			`UPDATE job_outbox SET claim_token = NULL, claimed_until = NULL WHERE claim_token = $1`, token); rerr != nil {
			return 0, errors.Join(err, fmt.Errorf("database: relay: release claim: %w", rerr))
		}
		return 0, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("database: relay: begin: %w", err)
	}
	defer tx.Rollback() // does nothing once Commit has succeeded

	rows, err = tx.QueryContext(ctx, `DELETE FROM job_outbox WHERE claim_token = $1 RETURNING job_id`, token)
	if err != nil {
		return 0, fmt.Errorf("database: relay: delete: %w", err)
	}
	var finished []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("database: relay: delete: %w", err)
		}
		finished = append(finished, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("database: relay: delete: %w", err)
	}
	if len(finished) == 0 {
		return 0, nil // every row was deleted with its job, or taken by another pass
	}
	// Only pending jobs move to queued; a job a worker already picked up
	// (Phase 6) must not go back. created_at comes from the API's clock and
	// now() from the database's; GREATEST keeps updated_at >= created_at
	// (a CHECK constraint) even if the two clocks disagree.
	rows, err = tx.QueryContext(ctx,
		`UPDATE jobs SET version = version + 1, status = 'queued', updated_at = GREATEST(now(), created_at) WHERE id = ANY($1) AND status = 'pending'
		 RETURNING `+jobColumns, finished)
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
	return len(finished), nil
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
//
// It reads the jobs without locking them, and holds no lock while send
// talks to Redis, so a worker can claim one of them meanwhile (Phase 17;
// FAILURES.md, case 66). Afterwards it touches only the jobs whose version
// is still the one it read: every write to a job raises its version (and
// most move updated_at), so a job claimed, finished, deleted or re-sent by
// another run in between keeps its newer state.
func (s *OutboxStore) RequeueStale(ctx context.Context, olderThan time.Duration, limit int, send func(ctx context.Context, jobIDs []string) error) (int, error) {
	// make_interval takes seconds as a double; olderThan never comes from users.
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, version FROM jobs
		 WHERE (status = 'queued' AND updated_at < now() - make_interval(secs => $1))
		    OR (status = 'retrying' AND next_attempt_at < now() - make_interval(secs => $1))
		 ORDER BY updated_at, id LIMIT $2`,
		olderThan.Seconds(), limit)
	if err != nil {
		return 0, fmt.Errorf("database: requeue stale: select: %w", err)
	}
	var ids []string
	var versions []int64
	for rows.Next() {
		var id string
		var v int64
		if err := rows.Scan(&id, &v); err != nil {
			rows.Close()
			return 0, fmt.Errorf("database: requeue stale: scan: %w", err)
		}
		ids = append(ids, id)
		versions = append(versions, v)
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
	if _, err := s.db.ExecContext(ctx,
		`UPDATE jobs AS j SET version = j.version + 1, updated_at = GREATEST(now(), j.created_at),
		        next_attempt_at = CASE WHEN j.status = 'retrying' THEN now() ELSE j.next_attempt_at END
		 FROM unnest($1::text[], $2::bigint[]) AS seen(id, version)
		 WHERE j.id = seen.id AND j.version = seen.version AND j.status IN ('queued', 'retrying')`, ids, versions); err != nil {
		return 0, fmt.Errorf("database: requeue stale: touch: %w", err)
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
