package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/work-frame/goflow/internal/job"
)

// uniqueViolation is PostgreSQL's SQLSTATE code for a duplicate key.
const uniqueViolation = "23505"

const selectJobColumns = `SELECT id, title, status, created_at, updated_at FROM jobs`

// JobStore keeps jobs in PostgreSQL. It is safe for concurrent use because *sql.DB is.
// Callers only ever see job package errors, never SQL or driver errors to decode.
type JobStore struct {
	db *sql.DB
}

// NewJobStore returns a JobStore that uses db.
func NewJobStore(db *sql.DB) *JobStore {
	return &JobStore{db: db}
}

// Add inserts j. It returns job.ErrDuplicateID if a job with the same ID exists.
func (s *JobStore) Add(ctx context.Context, j *job.Job) error {
	if j == nil {
		return job.ErrNilJob
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs (id, title, status, created_at, updated_at) VALUES ($1, $2, $3, $4, $5)`,
		j.ID, j.Title, string(j.Status), j.CreatedAt, j.UpdatedAt,
	)

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return job.ErrDuplicateID
	}
	if err != nil {
		return fmt.Errorf("database: add job %s: %w", j.ID, err)
	}
	return nil
}

// Get returns the job with the given ID, or job.ErrNotFound.
func (s *JobStore) Get(ctx context.Context, id string) (*job.Job, error) {
	row := s.db.QueryRowContext(ctx, selectJobColumns+` WHERE id = $1`, id)

	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, job.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("database: get job %s: %w", id, err)
	}
	return j, nil
}

// List returns jobs oldest first. An empty status means all jobs.
// It never returns a nil slice.
func (s *JobStore) List(ctx context.Context, status job.Status) ([]*job.Job, error) {
	const order = ` ORDER BY created_at, id`

	// Two separate queries rather than "WHERE $1 = '' OR status = $1": the OR form
	// makes it hard for Postgres to use the (status, created_at) index.
	var (
		rows *sql.Rows
		err  error
	)
	if status == "" {
		rows, err = s.db.QueryContext(ctx, selectJobColumns+order)
	} else {
		rows, err = s.db.QueryContext(ctx, selectJobColumns+` WHERE status = $1`+order, string(status))
	}
	if err != nil {
		return nil, fmt.Errorf("database: list jobs: %w", err)
	}
	// Returns the connection to the pool. Forgetting this leaks connections
	// until the pool's 10 are all stuck and every request waits forever.
	defer rows.Close()

	jobs := []*job.Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("database: scan job: %w", err)
		}
		jobs = append(jobs, j)
	}
	// rows.Next returns false on both "no more rows" and "error mid-stream". Err tells them apart.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("database: list jobs: %w", err)
	}
	return jobs, nil
}

// Delete removes the job with the given ID, or returns job.ErrNotFound.
func (s *JobStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE id = $1`, id)
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
	return nil
}

// scanner is the one method *sql.Row and *sql.Rows have in common,
// so scanJob works for both Get and List.
type scanner interface {
	Scan(dest ...any) error
}

func scanJob(s scanner) (*job.Job, error) {
	var j job.Job
	if err := s.Scan(&j.ID, &j.Title, &j.Status, &j.CreatedAt, &j.UpdatedAt); err != nil {
		return nil, err
	}
	// Postgres stores the instant. The driver returns it in the machine's local zone,
	// so normalise to UTC to match everything else in GoFlow.
	j.CreatedAt = j.CreatedAt.UTC()
	j.UpdatedAt = j.UpdatedAt.UTC()
	return &j, nil
}
