package relay

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// LeaseStore is what the lease reaper needs from the database.
// *database.WorkStore satisfies it.
type LeaseStore interface {
	// Reclaim takes the job back if its lease has run out by the
	// database's clock: requeue means it is queued again. Otherwise
	// leasedUntil says until when it is still leased (zero: the job isn't
	// processing at all).
	Reclaim(ctx context.Context, id string) (requeue bool, leasedUntil time.Time, err error)
	// ReclaimExpired takes back up to limit expired leases and returns the
	// IDs that are queued again.
	ReclaimExpired(ctx context.Context, limit int) ([]string, error)
}

// LeaseQueue is what the lease reaper needs from Redis. *queue.Queue
// satisfies it.
type LeaseQueue interface {
	Queue
	ExpiredLeases(ctx context.Context, now time.Time, limit int) ([]string, error)
	TrackLease(ctx context.Context, jobID string, until time.Time) error
	DropLease(ctx context.Context, jobID string) error
}

// LeaseReaper takes back jobs whose worker stopped renewing its lease, and
// sends them to the queue again.
//
// Redis keeps every lease's deadline (a sorted set). Every `every`, the
// reaper asks Redis which deadlines have passed; for each one it asks
// PostgreSQL, which takes the job back only if the lease has run out by
// ITS clock. Redis never decides: a deadline there may be stale (the lease
// was renewed meanwhile) or measured against a clock that runs fast. When
// no lease has passed its deadline, PostgreSQL isn't touched at all, so a
// sleeping free database stays asleep.
//
// Redis can lose the deadlines (a free Key Value restart). So at start and
// then every sweepEvery, the reaper also asks PostgreSQL directly for
// expired leases.
//
// Several reapers (the API and every cmd/worker run one) can work at once:
// PostgreSQL lets exactly one of them take a job back.
type LeaseReaper struct {
	store      LeaseStore
	queue      LeaseQueue
	logger     *slog.Logger
	every      time.Duration
	sweepEvery time.Duration
}

// NewLeaseReaper returns a reaper that checks Redis every `every` and
// sweeps PostgreSQL at start and every sweepEvery.
func NewLeaseReaper(store LeaseStore, queue LeaseQueue, logger *slog.Logger, every, sweepEvery time.Duration) *LeaseReaper {
	return &LeaseReaper{store: store, queue: queue, logger: logger, every: every, sweepEvery: sweepEvery}
}

// Start runs the reaper until ctx ends or stop is called.
func (r *LeaseReaper) Start(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		r.sweep(ctx)
		check := time.NewTicker(r.every)
		defer check.Stop()
		sweep := time.NewTicker(r.sweepEvery)
		defer sweep.Stop()
		failing := false
		for {
			select {
			case <-ctx.Done():
				return
			case <-sweep.C:
				r.sweep(ctx)
			case <-check.C:
				err := r.reap(ctx)
				switch {
				case err == nil:
					failing = false
				case errors.Is(err, context.Canceled):
				case !failing:
					// Log the first failure only, not one line every few seconds.
					r.logger.Warn("lease reaper: can't read lease deadlines from Redis; the hourly sweep still runs", "err", err)
					failing = true
				}
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

// reap asks PostgreSQL about every lease whose Redis deadline has passed.
func (r *LeaseReaper) reap(ctx context.Context) error {
	ids, err := r.queue.ExpiredLeases(ctx, time.Now(), BatchSize)
	if err != nil {
		return err
	}
	for _, id := range ids {
		requeue, until, err := r.store.Reclaim(ctx, id)
		switch {
		case err != nil:
			// Keep the deadline: the next check asks again.
			r.logger.Warn("lease reaper: can't check a lease", "job_id", id, "err", err)
		case requeue:
			r.logger.Info("lease reaper: lease ran out; job sent to run again", "job_id", id)
			r.send(ctx, id)
		case !until.IsZero():
			// Renewed meanwhile, or this clock runs ahead: PostgreSQL's
			// deadline replaces the stale one.
			if err := r.queue.TrackLease(ctx, id, until); err != nil {
				r.logger.Warn("lease reaper: can't update a lease deadline", "job_id", id, "err", err)
			}
		default:
			// Not processing any more: finished, deleted, or taken back by
			// another reaper (or out of attempts and now dead).
			r.drop(ctx, id)
		}
	}
	return nil
}

// sweep takes back every expired lease PostgreSQL knows about, in batches.
func (r *LeaseReaper) sweep(ctx context.Context) {
	total := 0
	for {
		ids, err := r.store.ReclaimExpired(ctx, BatchSize)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				r.logger.Warn("lease reaper: sweep failed; trying again at the next one", "err", err)
			}
			return
		}
		for _, id := range ids {
			r.send(ctx, id)
		}
		total += len(ids)
		if len(ids) < BatchSize {
			break
		}
	}
	if total > 0 {
		r.logger.Info("lease reaper: sweep sent jobs with expired leases to run again", "jobs", total)
	}
}

// send forgets a taken-back job's deadline, then puts its ID on the queue.
// In that order: once the ID is queued, a worker may claim the job and
// record a new deadline at once, and dropping after that would erase it.
// (A dropped deadline is restored by the holder's next heartbeat anyway.)
// If Redis fails, the job is 'queued' in PostgreSQL and the reconciler
// re-sends it later.
func (r *LeaseReaper) send(ctx context.Context, id string) {
	r.drop(ctx, id)
	if err := r.queue.Enqueue(ctx, id); err != nil {
		r.logger.Warn("lease reaper: job taken back but not queued; the reconciler will send it", "job_id", id, "err", err)
	}
}

func (r *LeaseReaper) drop(ctx context.Context, id string) {
	if err := r.queue.DropLease(ctx, id); err != nil {
		r.logger.Warn("lease reaper: can't drop a lease deadline", "job_id", id, "err", err)
	}
}
