package relay

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Delayed is what the promoter needs from Redis. *queue.Queue satisfies it.
type Delayed interface {
	PromoteDue(ctx context.Context, now time.Time, limit int) (int, error)
}

// Promoter moves retries that are due from Redis's delayed set to the
// pending list, once per interval.
//
// It only talks to Redis, never to PostgreSQL, so it can run every second
// without waking a sleeping database. A free Render Key Value has no command
// limit; one small script per second is nothing to it.
type Promoter struct {
	queue  Delayed
	logger *slog.Logger
	every  time.Duration
}

// NewPromoter returns a promoter that checks for due retries every `every`.
func NewPromoter(queue Delayed, logger *slog.Logger, every time.Duration) *Promoter {
	return &Promoter{queue: queue, logger: logger, every: every}
}

// Start runs the promoter until ctx ends or stop is called.
func (p *Promoter) Start(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		ticker := time.NewTicker(p.every)
		defer ticker.Stop()
		failing := false
		for {
			err := p.promote(ctx)
			switch {
			case err == nil:
				failing = false
			case errors.Is(err, context.Canceled):
			case !failing:
				// Log the first failure only, not one line a second.
				p.logger.Warn("promoter: can't reach Redis; retries wait in the delayed set", "err", err)
				failing = true
			}
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

// promote moves due IDs in batches until fewer than a batch are due.
func (p *Promoter) promote(ctx context.Context) error {
	for {
		n, err := p.queue.PromoteDue(ctx, time.Now(), BatchSize)
		if err != nil {
			return err
		}
		if n > 0 {
			p.logger.Info("promoter: retries due, queued again", "count", n)
		}
		if n < BatchSize {
			return nil
		}
	}
}
