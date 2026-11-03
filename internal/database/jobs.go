package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/work-frame/goflow/internal/job"
)

// uniqueViolation is PostgreSQL's SQLSTATE code for a duplicate key.
const uniqueViolation = "23505"

// idempotencyKeyIndex is the unique index from migration 0008. A unique
// violation names the index it hit, which tells a reused idempotency key
// apart from a duplicate job ID (FAILURES.md, cases 18 and 34).
const idempotencyKeyIndex = "jobs_user_idempotency_key"

const selectJobColumns = `SELECT ` + jobColumns + ` FROM jobs`

// JobStore keeps jobs in PostgreSQL. It is safe for concurrent use because *sql.DB is.
// Callers only ever see job package errors, never SQL or driver errors.
//
// Every read and delete takes the owner's user ID and puts it in the WHERE
// clause. Another user's job is never fetched at all, so it can't leak through
// a forgotten check in a handler: to the caller it simply does not exist.
type JobStore struct {
	db     *sql.DB
	notify Notifier // nil: nobody is told about changes
}

// NewJobStore returns a JobStore that uses db.
func NewJobStore(db *sql.DB) *JobStore {
	return &JobStore{db: db}
}

// WithNotifier returns a copy of s that tells n about every job it creates,
// re-runs or deletes, after the change is committed.
func (s *JobStore) WithNotifier(n Notifier) *JobStore {
	c := *s
	c.notify = n
	return &c
}

// Add inserts j together with its job_outbox row, in one transaction:
// either the job exists and will be queued, or neither row exists.
// It returns job.ErrNoOwner if j has no owner, job.ErrDuplicateID if a
// job with the same ID exists, and job.ErrDuplicateKey if the owner already
// has a job with j.IdempotencyKey (fetch that one with GetByIdempotencyKey).
func (s *JobStore) Add(ctx context.Context, j *job.Job) error {
	if j == nil {
		return job.ErrNilJob
	}
	if j.UserID == "" {
		return job.ErrNoOwner
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("database: add job %s: begin: %w", j.ID, err)
	}
	defer tx.Rollback() // does nothing once Commit has succeeded

	_, err = tx.ExecContext(ctx,
		`INSERT INTO jobs (id, user_id, title, status, created_at, updated_at, max_attempts, idempotency_key)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		j.ID, j.UserID, j.Title, string(j.Status), j.CreatedAt, j.UpdatedAt, maxAttempts(j), nullIfEmpty(j.IdempotencyKey),
	)
	// With a key, a second request waits here until the first one's
	// transaction ends, then fails with this error if it committed.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		if pgErr.ConstraintName == idempotencyKeyIndex {
			return job.ErrDuplicateKey
		}
		return job.ErrDuplicateID
	}
	if err != nil {
		return fmt.Errorf("database: add job %s: %w", j.ID, err)
	}

	// Same transaction (tx, not s.db): if this insert fails, the job above
	// is rolled back with it.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO job_outbox (job_id, created_at) VALUES ($1, $2)`, j.ID, j.CreatedAt,
	); err != nil {
		return fmt.Errorf("database: add job %s: outbox: %w", j.ID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("database: add job %s: commit: %w", j.ID, err)
	}
	notifyChanged(ctx, s.notify, j)
	return nil
}

// Get returns the job with the given ID if userID owns it, or job.ErrNotFound.
// "Owned by someone else" and "doesn't exist" are the same answer on purpose.
func (s *JobStore) Get(ctx context.Context, userID, id string) (*job.Job, error) {
	row := s.db.QueryRowContext(ctx, selectJobColumns+` WHERE id = $1 AND user_id = $2`, id, userID)

	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, job.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("database: get job %s: %w", id, err)
	}
	return j, nil
}

// GetByIdempotencyKey returns userID's job created with key, or
// job.ErrNotFound. Like Get, it never finds another user's job.
func (s *JobStore) GetByIdempotencyKey(ctx context.Context, userID, key string) (*job.Job, error) {
	row := s.db.QueryRowContext(ctx, selectJobColumns+` WHERE user_id = $1 AND idempotency_key = $2`, userID, key)

	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, job.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("database: get job by idempotency key: %w", err)
	}
	j.IdempotencyKey = key
	return j, nil
}

// Rerun gives a dead (or old failed) job a fresh set of attempts: it goes
// back to pending with attempts 0 and no error, and gets a new outbox row,
// all in one transaction, so the relay delivers it like a new job. It
// returns the updated job, job.ErrNotFound if userID has no such job, or
// job.ErrNotRerunnable if the job isn't dead or failed.
//
// Two concurrent Reruns of one job can't both win: the UPDATE locks the
// row, the second waits, re-checks the status (now pending) and changes
// nothing. The same row-lock re-check that makes Claim safe (case 28).
func (s *JobStore) Rerun(ctx context.Context, userID, id string) (*job.Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("database: rerun job %s: begin: %w", id, err)
	}
	defer tx.Rollback() // does nothing once Commit has succeeded

	row := tx.QueryRowContext(ctx,
		`UPDATE jobs SET status = 'pending', attempts = 0, next_attempt_at = NULL, last_error = NULL,
		        updated_at = GREATEST(now(), created_at)
		 WHERE id = $1 AND user_id = $2 AND status IN ('dead', 'failed')
		 RETURNING `+jobColumns, id, userID)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		// Nothing changed. Missing (or someone else's), or the wrong status?
		// Asked inside the same transaction, so it holds no second connection.
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM jobs WHERE id = $1 AND user_id = $2)`, id, userID,
		).Scan(&exists); err != nil {
			return nil, fmt.Errorf("database: rerun job %s: %w", id, err)
		}
		if !exists {
			return nil, job.ErrNotFound
		}
		return nil, job.ErrNotRerunnable
	}
	if err != nil {
		return nil, fmt.Errorf("database: rerun job %s: %w", id, err)
	}

	// A dead job has no outbox row (the relay deleted it long ago), but
	// DO NOTHING keeps this safe if one is somehow still there.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO job_outbox (job_id, created_at) VALUES ($1, now()) ON CONFLICT (job_id) DO NOTHING`, id,
	); err != nil {
		return nil, fmt.Errorf("database: rerun job %s: outbox: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("database: rerun job %s: commit: %w", id, err)
	}
	notifyChanged(ctx, s.notify, j)
	return j, nil
}

// List returns one page of userID's jobs, oldest first by (created_at, id).
// It uses keyset pagination: "rows after this cursor", never OFFSET.
func (s *JobStore) List(ctx context.Context, userID string, p job.ListParams) (job.Page, error) {
	if err := p.Validate(); err != nil {
		return job.Page{}, err
	}

	// The SQL is built only from fixed strings written here. The $N numbers vary,
	// but every value travels separately in args, so this is not injectable.
	args := []any{userID}
	where := []string{"user_id = $1"}
	if p.Status != "" {
		args = append(args, string(p.Status))
		where = append(where, fmt.Sprintf("status = $%d", len(args)))
	}
	if p.After != nil {
		args = append(args, p.After.CreatedAt, p.After.ID)
		where = append(where, fmt.Sprintf("(created_at, id) > ($%d, $%d)", len(args)-1, len(args)))
	}

	query := selectJobColumns + " WHERE " + strings.Join(where, " AND ")
	// One extra row tells NewPage whether another page exists.
	args = append(args, p.Limit+1)
	query += fmt.Sprintf(" ORDER BY created_at, id LIMIT $%d", len(args))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return job.Page{}, fmt.Errorf("database: list jobs: %w", err)
	}
	defer rows.Close()

	jobs := make([]*job.Job, 0, p.Limit+1)
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return job.Page{}, fmt.Errorf("database: scan job: %w", err)
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return job.Page{}, fmt.Errorf("database: list jobs: %w", err)
	}
	return job.NewPage(jobs, p.Limit), nil
}

// Delete removes the job with the given ID if userID owns it, or returns job.ErrNotFound.
func (s *JobStore) Delete(ctx context.Context, userID, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return fmt.Errorf("database: delete job %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("database: delete job %s: %w", id, err)
	}
	if n == 0 {
		return job.ErrNotFound
	}
	if s.notify != nil {
		s.notify.JobDeleted(ctx, userID, id)
	}
	return nil
}

// scanner is the one method *sql.Row and *sql.Rows have in common.
type scanner interface {
	Scan(dest ...any) error
}

func scanJob(s scanner) (*job.Job, error) {
	var j job.Job
	var next sql.NullTime
	if err := s.Scan(&j.ID, &j.UserID, &j.Title, &j.Status, &j.CreatedAt, &j.UpdatedAt, &j.Attempts, &j.MaxAttempts, &next); err != nil {
		return nil, err
	}
	j.CreatedAt = j.CreatedAt.UTC()
	j.UpdatedAt = j.UpdatedAt.UTC()
	if next.Valid {
		t := next.Time.UTC()
		j.NextAttemptAt = &t
	}
	return &j, nil
}

// maxAttempts is j's limit, or the default for a job built without job.New.
func maxAttempts(j *job.Job) int {
	if j.MaxAttempts > 0 {
		return j.MaxAttempts
	}
	return job.DefaultMaxAttempts
}

// nullIfEmpty stores "" as NULL, so jobs without a key never collide in
// the unique index.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
