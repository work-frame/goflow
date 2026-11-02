// Package worker takes job IDs off the queue and runs them, one at a time.
//
// The order of steps is what keeps a job from being lost or half-recorded:
//
//	Take     ID moves pending → processing (Redis)
//	Claim    job becomes processing (PostgreSQL); finished or missing → skip
//	Handle   the work itself
//	Complete job becomes completed, or Fail → failed (PostgreSQL)
//	Ack      ID leaves processing (Redis)
//
// A crash before Ack leaves the ID in the processing list. On the next start
// Recover puts it back, and Claim decides whether it still needs running.
// So a job may run twice, but it is never silently dropped.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/work-frame/goflow/internal/queue"
)

// takeTimeout is how long one Take waits for a job before checking whether
// the worker should stop.
const takeTimeout = 5 * time.Second

// Queue is what the worker needs from Redis. *queue.Queue satisfies it.
type Queue interface {
	Take(ctx context.Context, timeout time.Duration) (string, error)
	Ack(ctx context.Context, jobID string) error
	Recover(ctx context.Context) (int, error)
}

// Store is what the worker needs from PostgreSQL. *database.WorkStore satisfies it.
type Store interface {
	Claim(ctx context.Context, id string) (bool, error)
	Complete(ctx context.Context, id string) error
	Fail(ctx context.Context, id string) error
}

// Handler does the work for one job. Real job types come later; for now the
// worker program passes a placeholder that only waits.
type Handler func(ctx context.Context, jobID string) error

// Worker processes jobs until its context ends.
type Worker struct {
	Queue  Queue
	Store  Store
	Handle Handler
	Logger *slog.Logger
}

// Run recovers unfinished IDs, then processes jobs until ctx is cancelled.
// It returns nil on a normal stop and an error only if it can't start.
func (w *Worker) Run(ctx context.Context) error {
	n, err := w.Queue.Recover(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		w.Logger.Info("worker: unfinished jobs put back in the queue", "count", n)
	}

	for ctx.Err() == nil {
		id, err := w.Queue.Take(ctx, takeTimeout)
		switch {
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, queue.ErrEmpty):
			continue
		case err != nil:
			w.Logger.Warn("worker: can't reach the queue, retrying", "err", err)
			sleep(ctx, time.Second)
			continue
		}
		w.process(ctx, id)
	}
	return nil
}

// process runs one job. Errors are logged, never fatal: the ID stays in the
// processing list unless the job's outcome was saved, and Recover picks it
// up on the next start.
func (w *Worker) process(ctx context.Context, id string) {
	log := w.Logger.With("job_id", id)

	run, err := w.Store.Claim(ctx, id)
	if err != nil {
		log.Warn("worker: claim failed; the ID stays in processing", "err", err)
		return
	}
	if !run {
		log.Info("worker: skipping, already finished or deleted")
		w.ack(ctx, log, id)
		return
	}

	start := time.Now()
	err = w.Handle(ctx, id)
	if ctx.Err() != nil {
		// Stopping mid-job: record nothing, keep the ID. It runs again after
		// the next start, which is better than calling it done or failed.
		log.Info("worker: stopped mid-job; it will run again after restart")
		return
	}

	// The outcome is saved with a fresh context: the job's own deadline
	// mustn't stop us from recording how it ended.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err != nil {
		log.Warn("worker: job failed", "err", err)
		if err := w.Store.Fail(saveCtx, id); err != nil {
			log.Warn("worker: couldn't record the failure; the ID stays in processing", "err", err)
			return
		}
	} else {
		if err := w.Store.Complete(saveCtx, id); err != nil {
			log.Warn("worker: couldn't record completion; the ID stays in processing", "err", err)
			return
		}
		log.Info("worker: job completed", "duration", time.Since(start).Round(time.Millisecond))
	}
	w.ack(saveCtx, log, id)
}

func (w *Worker) ack(ctx context.Context, log *slog.Logger, id string) {
	if err := w.Queue.Ack(ctx, id); err != nil {
		// Harmless: after a restart Recover puts it back and Claim skips it.
		log.Warn("worker: ack failed; the job may be seen again and skipped", "err", err)
	}
}

// sleep waits d or until ctx ends, whichever comes first.
func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
