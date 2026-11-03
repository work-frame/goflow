package database

import (
	"context"

	"github.com/work-frame/goflow/internal/job"
)

// Notifier hears about job changes after they are committed.
// events.Publisher satisfies it; tests use a recorder.
//
// Stores call it only after COMMIT succeeds, so a listener never hears
// about a change that was rolled back. It must return at once and can't
// fail: the change is already safe in PostgreSQL, and the notification is
// only a hint to whoever is watching (see internal/events).
//
// Give a store a Notifier only when there is one: a nil *events.Publisher
// stored in this interface is not a nil interface, and the first change
// would call a method on nil (FAILURES.md, case 25).
type Notifier interface {
	JobChanged(ctx context.Context, j *job.Job)
	JobDeleted(ctx context.Context, userID, id string)
}

// jobColumns is every column scanJob reads, in its order. Status-changing
// statements return them (RETURNING), so the notification carries the job
// exactly as committed without a second query.
const jobColumns = `id, user_id, title, status, created_at, updated_at, attempts, max_attempts, next_attempt_at, version`

func notifyChanged(ctx context.Context, n Notifier, jobs ...*job.Job) {
	if n == nil {
		return
	}
	for _, j := range jobs {
		n.JobChanged(ctx, j)
	}
}

// scanJobs reads every row of rows with scanJob and closes rows.
func scanJobs(rows interface {
	scanner
	Next() bool
	Err() error
	Close() error
}) ([]*job.Job, error) {
	defer rows.Close()
	var out []*job.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
