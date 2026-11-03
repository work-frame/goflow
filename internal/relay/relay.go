// Package relay moves job IDs from the PostgreSQL outbox to the Redis queue,
// and puts back IDs Redis has lost.
//
// The API never talks to Redis while handling a request. It writes the job
// and an outbox row in one transaction, then wakes the relay. The relay
// sends whatever is waiting, and tries again later if Redis is down.
//
// Neither loop polls the database while all is well. On a free database
// that sleeps when idle (Neon suspends after 5 quiet minutes and has a
// monthly compute budget), a query every few seconds would keep it awake
// around the clock. So the relay runs at start, when woken, and after a
// failure; the reconciler runs at start and then once an hour.
package relay

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

const (
	// BatchSize is how many IDs one transaction sends at most.
	BatchSize = 100
	// maxRetry caps the wait between attempts while Redis is unreachable.
	maxRetry = 5 * time.Minute
)

// Outbox is what the relay needs from the database. *database.OutboxStore
// satisfies it.
type Outbox interface {
	Relay(ctx context.Context, limit int, send func(ctx context.Context, jobIDs []string) error) (int, error)
}

// Queue is where the IDs go. *queue.Queue satisfies it.
type Queue interface {
	Enqueue(ctx context.Context, jobIDs ...string) error
}

// Relay runs one background loop. Create it with New, start it with Start.
type Relay struct {
	outbox   Outbox
	queue    Queue
	logger   *slog.Logger
	minRetry time.Duration
	// wake has room for one signal. Wake never blocks: if a signal is already
	// waiting, another one adds nothing, because the next pass sends
	// everything that is waiting anyway.
	wake chan struct{}
}

// New returns a relay. After a failed pass it waits minRetry, then twice as
// long after each further failure, up to 5 minutes.
func New(outbox Outbox, queue Queue, logger *slog.Logger, minRetry time.Duration) *Relay {
	return &Relay{outbox: outbox, queue: queue, logger: logger, minRetry: minRetry, wake: make(chan struct{}, 1)}
}

// Wake asks the relay to check the outbox now. It never blocks, so the API
// can call it on every new job.
func (r *Relay) Wake() {
	select {
	case r.wake <- struct{}{}:
	default: // a wake-up is already pending
	}
}

// Start runs the loop in a goroutine until ctx ends or stop is called.
// stop waits for a pass that is running to finish, so the database and
// the queue can be closed safely afterwards.
//
// The loop checks the outbox once at start, then only when woken, or on a
// retry timer after a failed pass. While everything works it doesn't touch
// the database at all between jobs.
func (r *Relay) Start(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		var retry time.Duration // 0 while healthy
		for {
			var timer *time.Timer
			var retryAfter <-chan time.Time // nil while healthy: never fires
			if r.drain(ctx) {
				retry = 0
			} else {
				retry = nextRetry(retry, r.minRetry)
				timer = time.NewTimer(retry)
				retryAfter = timer.C
			}
			select {
			case <-ctx.Done():
			case <-retryAfter:
			case <-r.wake:
			}
			if timer != nil {
				timer.Stop()
			}
			if ctx.Err() != nil {
				return
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

// nextRetry doubles the wait, starting at first, capped at maxRetry.
func nextRetry(current, first time.Duration) time.Duration {
	if current == 0 {
		return first
	}
	return min(current*2, maxRetry)
}

// drain sends batches until the outbox is empty. It reports false if a pass
// failed and the outbox should be tried again later.
func (r *Relay) drain(ctx context.Context) bool {
	for {
		n, err := r.pass(ctx)
		switch {
		case errors.Is(err, context.Canceled):
			return true // shutting down: not a failure
		case err != nil:
			// The rows stay in the outbox; the retry timer tries again.
			r.logger.Warn("relay: jobs not queued yet, will retry", "err", err)
			return false
		case n > 0:
			r.logger.Info("relay: jobs queued", "count", n)
		}
		if n < BatchSize {
			return true
		}
	}
}

func (r *Relay) pass(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return r.outbox.Relay(ctx, BatchSize, func(ctx context.Context, ids []string) error {
		return r.queue.Enqueue(ctx, ids...)
	})
}
