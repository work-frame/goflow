// Package relay moves job IDs from the PostgreSQL outbox to the Redis queue.
//
// The API never talks to Redis while handling a request. It writes the job
// and an outbox row in one transaction, then wakes the relay. The relay
// sends whatever is waiting, and tries again later if Redis is down.
package relay

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// BatchSize is how many IDs one transaction sends at most.
const BatchSize = 100

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
	interval time.Duration
	// wake has room for one signal. Wake never blocks: if a signal is already
	// waiting, another one adds nothing, because the next pass sends
	// everything that is waiting anyway.
	wake chan struct{}
}

// New returns a relay that checks the outbox when woken, and at least once
// every interval. The interval is the backstop for wake-ups that never
// came: a send that failed, another instance's jobs, a restart.
func New(outbox Outbox, queue Queue, logger *slog.Logger, interval time.Duration) *Relay {
	return &Relay{outbox: outbox, queue: queue, logger: logger, interval: interval, wake: make(chan struct{}, 1)}
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
func (r *Relay) Start(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		for {
			r.drain(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-r.wake:
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

// drain sends batches until the outbox is empty or a send fails.
func (r *Relay) drain(ctx context.Context) {
	for {
		n, err := r.pass(ctx)
		switch {
		case errors.Is(err, context.Canceled):
			return // shutting down: not a failure
		case err != nil:
			// The rows stay in the outbox; the next tick tries again.
			r.logger.Warn("relay: jobs not queued yet, will retry", "err", err)
			return
		case n > 0:
			r.logger.Info("relay: jobs queued", "count", n)
		}
		if n < BatchSize {
			return
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
