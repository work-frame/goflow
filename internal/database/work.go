package database

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/work-frame/goflow/internal/job"
)

// WorkStore is what workers need from the database: claim a job under a
// lease, keep the lease alive, record how the job ended, and take back jobs
// whose lease ran out. Unlike JobStore it isn't scoped to a user. Workers
// run every user's jobs, and only ever by an ID taken from the queue.
//
// # Leases
//
// A 'processing' job belongs to whoever holds its lease: a random token
// written by Claim, valid until lease_expires_at. Two rules make several
// worker processes safe together:
//
//   - The token fences every write. Renew, Complete, Retry and Bury change
//     the job only if the caller's token is still the job's token. Once a
//     lease is taken back (Reclaim) and the job claimed again, the old
//     holder's token matches nothing: whatever it tries to record is
//     refused with ErrLeaseLost, and it must stop.
//   - The DATABASE's clock decides expiry. Every lease time is computed
//     with now() inside PostgreSQL, and Reclaim takes a job back only if
//     lease_expires_at < now() there. Worker clocks, and the deadlines Redis
//     keeps, only say when to ask.
//
// An expired lease that nobody has taken back yet still counts: its holder
// can renew it or record a result, because no other worker can have the
// job. Expiry only makes a lease eligible to be taken; the token says who
// has it.
type WorkStore struct {
	db     *sql.DB
	notify Notifier // nil: nobody is told about changes
}

// NewWorkStore returns a WorkStore that uses db.
func NewWorkStore(db *sql.DB) *WorkStore {
	return &WorkStore{db: db}
}

// WithNotifier returns a copy of s that tells n about every status change it
// makes. Each change is a single statement, committed when it returns, so
// n hears about it right after.
func (s *WorkStore) WithNotifier(n Notifier) *WorkStore {
	c := *s
	c.notify = n
	return &c
}

// Claimed is a won claim: the attempt and the lease to run it under.
type Claimed struct {
	Attempt     int // 1 for the first run
	MaxAttempts int
	Title       string
	Type        job.Type
	// UserID owns the job: a webhook is signed with the owner's secret.
	UserID string
	// Webhook is a webhook job's URL and body; nil for other types.
	Webhook *job.Webhook
	// LeaseToken must accompany every later write for this attempt.
	LeaseToken string
	// LeaseExpires is when the lease runs out unless renewed, on the
	// database's clock.
	LeaseExpires time.Time
}

// ErrLeaseLost means the caller's lease is no longer the job's lease: it
// ran out and the job was taken back (and maybe claimed by another worker),
// or the job was finished or deleted meanwhile. The caller must stop
// working on the job and record nothing.
var ErrLeaseLost = errors.New("lease lost: the job is not processing under this lease")

// Claim marks a pending, queued or retrying job processing under a new
// lease of length leaseFor, counts the attempt, and returns it. ok is false
// if this caller didn't win the job and must not run it.
//
// Two workers claiming the same job (a duplicate ID in the queue) can't both
// win, even in different processes: PostgreSQL locks the row for the first
// UPDATE, the second waits for that commit, then re-checks the WHERE clause,
// finds 'processing', and changes 0 rows.
//
// Every claim gets a fresh token, so two attempts of the same job, even by
// the same worker, never share one.
//
// A retrying job is claimable even if its next_attempt_at hasn't quite come:
// Redis decides when its ID is delivered, and the two clocks may differ by a
// second. Refusing it here would drop the ID for good.
func (s *WorkStore) Claim(ctx context.Context, id string, leaseFor time.Duration) (c Claimed, ok bool, err error) {
	token := rand.Text() // 26 characters, 130 random bits
	var expires time.Time
	var body []byte
	var j *job.Job
	j, err = scanJobAnd(s.db.QueryRowContext(ctx,
		`UPDATE jobs SET version = version + 1, status = 'processing', attempts = attempts + 1, next_attempt_at = NULL,
		        lease_token = $2, lease_expires_at = now() + make_interval(secs => $3),
		        updated_at = GREATEST(now(), created_at)
		 WHERE id = $1 AND status IN ('pending', 'queued', 'retrying')
		 RETURNING `+jobColumns+`, lease_expires_at, payload->'body'`, id, token, leaseFor.Seconds()), &expires, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return Claimed{}, false, nil // finished, dead, deleted or someone else's
	}
	if err != nil {
		return Claimed{}, false, fmt.Errorf("database: claim job %s: %w", id, err)
	}
	notifyChanged(ctx, s.notify, j)
	c = Claimed{Attempt: j.Attempts, MaxAttempts: j.MaxAttempts, Title: j.Title, Type: j.Type, UserID: j.UserID,
		LeaseToken: token, LeaseExpires: expires.UTC()}
	if j.Webhook != nil {
		c.Webhook = &job.Webhook{URL: j.Webhook.URL, Body: body}
	}
	return c, true, nil
}

// Renew extends the lease to leaseFor from now (the database's now) and
// returns the new expiry. ErrLeaseLost means the lease is gone: the worker
// must cancel the job.
//
// It raises the version like every write to jobs, but tells nobody: the
// job's status hasn't changed, and the next real change carries a higher
// version anyway.
func (s *WorkStore) Renew(ctx context.Context, id, token string, leaseFor time.Duration) (time.Time, error) {
	var expires time.Time
	err := s.db.QueryRowContext(ctx,
		`UPDATE jobs SET version = version + 1, lease_expires_at = now() + make_interval(secs => $3)
		 WHERE id = $1 AND status = 'processing' AND lease_token = $2
		 RETURNING lease_expires_at`, id, token, leaseFor.Seconds()).Scan(&expires)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, fmt.Errorf("database: renew lease of job %s: %w", id, ErrLeaseLost)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("database: renew lease of job %s: %w", id, err)
	}
	return expires.UTC(), nil
}

// Complete marks a processing job completed and ends its lease.
func (s *WorkStore) Complete(ctx context.Context, id, token string) error {
	j, err := scanJob(s.db.QueryRowContext(ctx,
		`UPDATE jobs SET version = version + 1, status = 'completed', lease_token = NULL, lease_expires_at = NULL,
		        updated_at = GREATEST(now(), created_at)
		 WHERE id = $1 AND status = 'processing' AND lease_token = $2
		 RETURNING `+jobColumns, id, token))
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("database: complete job %s: %w", id, ErrLeaseLost)
	}
	if err != nil {
		return fmt.Errorf("database: complete job %s: %w", id, err)
	}
	notifyChanged(ctx, s.notify, j)
	return nil
}

// Retry records a failed attempt that will run again: the job becomes
// 'retrying', due after delay (counted on the database's clock), with its
// error kept for operators, and its lease ends. It returns when the retry
// is due, for the caller to schedule in Redis, which it must do only after
// this has committed.
func (s *WorkStore) Retry(ctx context.Context, id, token string, delay time.Duration, lastError string) (time.Time, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx,
		`UPDATE jobs SET version = version + 1, status = 'retrying', last_error = $4,
		        next_attempt_at = now() + make_interval(secs => $3),
		        lease_token = NULL, lease_expires_at = NULL,
		        updated_at = GREATEST(now(), created_at)
		 WHERE id = $1 AND status = 'processing' AND lease_token = $2
		 RETURNING `+jobColumns, id, token, delay.Seconds(), truncateError(lastError)))
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, fmt.Errorf("database: retry job %s: %w", id, ErrLeaseLost)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("database: retry job %s: %w", id, err)
	}
	notifyChanged(ctx, s.notify, j)
	return *j.NextAttemptAt, nil
}

// Bury marks a processing job dead (a permanent error, or out of attempts)
// and ends its lease.
func (s *WorkStore) Bury(ctx context.Context, id, token, lastError string) error {
	j, err := scanJob(s.db.QueryRowContext(ctx,
		`UPDATE jobs SET version = version + 1, status = 'dead', last_error = $3, next_attempt_at = NULL,
		        lease_token = NULL, lease_expires_at = NULL,
		        updated_at = GREATEST(now(), created_at)
		 WHERE id = $1 AND status = 'processing' AND lease_token = $2
		 RETURNING `+jobColumns, id, token, truncateError(lastError)))
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("database: bury job %s: %w", id, ErrLeaseLost)
	}
	if err != nil {
		return fmt.Errorf("database: bury job %s: %w", id, err)
	}
	notifyChanged(ctx, s.notify, j)
	return nil
}

// LostLeaseError is what a job whose last attempt lost its lease dies with.
const LostLeaseError = "lease expired: the worker running the last attempt stopped renewing it"

// reclaimSet is the change Reclaim and ReclaimExpired make: the job goes
// back to 'queued' to run again, or, if that was its last attempt (a lost
// lease uses one up, like a crash), to 'dead'. The lease ends either way.
const reclaimSet = `UPDATE jobs SET version = version + 1,
        status = CASE WHEN attempts >= max_attempts THEN 'dead' ELSE 'queued' END,
        last_error = CASE WHEN attempts >= max_attempts THEN '` + LostLeaseError + `' ELSE last_error END,
        next_attempt_at = NULL, lease_token = NULL, lease_expires_at = NULL,
        updated_at = GREATEST(now(), created_at)`

// Reclaim takes job id back if its lease has run out by the database's
// clock. requeue reports that the job is 'queued' again and its ID must be
// sent to the queue. If the lease hasn't run out (it was renewed, or the
// caller's clock is ahead), leasedUntil says until when, and nothing
// changes. If the job isn't processing at all (finished, deleted, already
// taken back), both are zero.
//
// Several reapers may call it for the same job at once: the row lock lets
// one take the job back, and the others find it no longer processing.
func (s *WorkStore) Reclaim(ctx context.Context, id string) (requeue bool, leasedUntil time.Time, err error) {
	j, err := scanJob(s.db.QueryRowContext(ctx,
		reclaimSet+`
		 WHERE id = $1 AND status = 'processing' AND lease_expires_at < now()
		 RETURNING `+jobColumns, id))
	if err == nil {
		notifyChanged(ctx, s.notify, j)
		return j.Status == job.StatusQueued, time.Time{}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, time.Time{}, fmt.Errorf("database: reclaim job %s: %w", id, err)
	}
	var until sql.NullTime
	err = s.db.QueryRowContext(ctx,
		`SELECT lease_expires_at FROM jobs WHERE id = $1 AND status = 'processing'`, id).Scan(&until)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !until.Valid) {
		return false, time.Time{}, nil
	}
	if err != nil {
		return false, time.Time{}, fmt.Errorf("database: reclaim job %s: %w", id, err)
	}
	return false, until.Time.UTC(), nil
}

// ReclaimExpired takes back up to limit jobs whose lease ran out, oldest
// first, and returns the IDs of those that went back to 'queued' (the
// caller sends them to the queue). It reads PostgreSQL only, so it finds
// expired leases even when Redis has lost its list of lease deadlines (a
// Key Value restart). Run it rarely: at start and hourly.
func (s *WorkStore) ReclaimExpired(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		reclaimSet+`
		 WHERE id IN (SELECT id FROM jobs WHERE status = 'processing' AND lease_expires_at < now()
		              ORDER BY lease_expires_at LIMIT $1 FOR UPDATE SKIP LOCKED)
		   AND status = 'processing' AND lease_expires_at < now()
		 RETURNING `+jobColumns, limit)
	if err != nil {
		return nil, fmt.Errorf("database: reclaim expired leases: %w", err)
	}
	taken, err := scanJobs(rows)
	if err != nil {
		return nil, fmt.Errorf("database: reclaim expired leases: %w", err)
	}
	notifyChanged(ctx, s.notify, taken...)
	var requeue []string
	for _, j := range taken {
		if j.Status == job.StatusQueued {
			requeue = append(requeue, j.ID)
		}
	}
	return requeue, nil
}

// Processing reports which of ids belong to jobs that are 'processing'
// right now. Startup cleanup uses it to leave other workers' jobs alone.
func (s *WorkStore) Processing(ctx context.Context, ids []string) (map[string]bool, error) {
	out := make(map[string]bool, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM jobs WHERE id = ANY($1) AND status = 'processing'`, ids)
	if err != nil {
		return nil, fmt.Errorf("database: processing jobs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("database: processing jobs: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

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
