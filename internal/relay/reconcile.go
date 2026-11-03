package relay

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// StaleStore is what the reconciler needs from the database.
// *database.OutboxStore satisfies it.
type StaleStore interface {
	RequeueStale(ctx context.Context, olderThan time.Duration, limit int, send func(ctx context.Context, jobIDs []string) error) (int, error)
}

// Reconciler re-sends the IDs of jobs that have been 'queued' for too long.
//
// The outbox guarantees an ID reaches Redis once. It can't notice when Redis
// later loses it: a free Render Key Value instance has no persistence, so a
// restart empties both lists. Those jobs then stay 'queued' in PostgreSQL
// with no ID anywhere, and nothing would ever run them. The reconciler finds
// them by age and pushes their IDs again.
//
// Pushing an ID that is still in Redis only makes a duplicate, and duplicates
// are harmless: Claim lets exactly one worker win the job, and the others
// skip it. That safety is what makes this blunt approach acceptable.
type Reconciler struct {
	store      StaleStore
	queue      Queue
	logger     *slog.Logger
	every      time.Duration
	staleAfter time.Duration
}

// NewReconciler returns a reconciler that runs at start and then every
// `every`, re-sending jobs queued longer than staleAfter.
func NewReconciler(store StaleStore, queue Queue, logger *slog.Logger, every, staleAfter time.Duration) *Reconciler {
	return &Reconciler{store: store, queue: queue, logger: logger, every: every, staleAfter: staleAfter}
}

// Start runs the reconciler in a goroutine until ctx ends or stop is called.
// stop waits for a pass that is running to finish.
func (r *Reconciler) Start(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		ticker := time.NewTicker(r.every)
		defer ticker.Stop()
		for {
			r.run(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

// run re-sends stale jobs in batches until none are left.
func (r *Reconciler) run(ctx context.Context) {
	total := 0
	for {
		n, err := r.pass(ctx)
		switch {
		case errors.Is(err, context.Canceled):
			return
		case err != nil:
			r.logger.Warn("reconciler: pass failed, will try at the next run", "err", err)
			return
		}
		total += n
		if n < BatchSize {
			if total > 0 {
				r.logger.Warn("reconciler: re-queued jobs that were stuck in 'queued'", "count", total)
			}
			return
		}
	}
}

func (r *Reconciler) pass(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return r.store.RequeueStale(ctx, r.staleAfter, BatchSize, func(ctx context.Context, ids []string) error {
		return r.queue.Enqueue(ctx, ids...)
	})
}
