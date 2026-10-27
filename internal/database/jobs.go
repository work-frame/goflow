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

const selectJobColumns = `SELECT id, user_id, title, status, created_at, updated_at FROM jobs`

// JobStore keeps jobs in PostgreSQL. It is safe for concurrent use because *sql.DB is.
// Callers only ever see job package errors, never SQL or driver errors.
//
// Every read and delete takes the owner's user ID and puts it in the WHERE
// clause. Another user's job is never fetched at all, so it can't leak through
// a forgotten check in a handler: to the caller it simply does not exist.
type JobStore struct {
	db *sql.DB
}

// NewJobStore returns a JobStore that uses db.
func NewJobStore(db *sql.DB) *JobStore {
	return &JobStore{db: db}
}

// Add inserts j. It returns job.ErrNoOwner if j has no owner and
// job.ErrDuplicateID if a job with the same ID exists.
func (s *JobStore) Add(ctx context.Context, j *job.Job) error {
	if j == nil {
		return job.ErrNilJob
	}
	if j.UserID == "" {
		return job.ErrNoOwner
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs (id, user_id, title, status, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		j.ID, j.UserID, j.Title, string(j.Status), j.CreatedAt, j.UpdatedAt,
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
	return nil
}

// scanner is the one method *sql.Row and *sql.Rows have in common.
type scanner interface {
	Scan(dest ...any) error
}

func scanJob(s scanner) (*job.Job, error) {
	var j job.Job
	if err := s.Scan(&j.ID, &j.UserID, &j.Title, &j.Status, &j.CreatedAt, &j.UpdatedAt); err != nil {
		return nil, err
	}
	j.CreatedAt = j.CreatedAt.UTC()
	j.UpdatedAt = j.UpdatedAt.UTC()
	return &j, nil
}
