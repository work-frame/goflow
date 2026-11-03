// Package worker runs a pool of goroutines that take job IDs off the queue
// and run the jobs.
//
// Each job goes through the same steps, in an order that keeps it from being
// lost or half-recorded:
//
//	Take     ID moves pending → processing (Redis)
//	Claim    pending/queued → processing (PostgreSQL); only one caller can win
//	Handle   the work itself
//	Complete job becomes completed, or Fail → failed (PostgreSQL)
//	Ack      ID leaves processing (Redis)
//
// A crash before Ack leaves the ID in Redis's processing list and the job
// processing in PostgreSQL. The next start puts both back (see Pool.Run).
// So a job may run twice, but it is never silently dropped.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/work-frame/goflow/internal/queue"
)

const (
	// takeTimeout is how long one Take waits for a job before the worker
	// checks whether it should stop. go-redis doesn't interrupt a blocking
	// BLMOVE when its context is cancelled, so this is also the longest an
	// idle worker delays shutdown. Shorter means faster shutdowns but more
	// idle commands to Redis (one per worker per timeout).
	takeTimeout = 2 * time.Second
	// abandonWait bounds the wait for handlers after their context is
	// cancelled. A handler that ignores its context can't hold the process
	// open forever.
	abandonWait = 5 * time.Second
)

// Queue is what the pool needs from Redis. *queue.Queue satisfies it.
type Queue interface {
	Take(ctx context.Context, timeout time.Duration) (string, error)
	Ack(ctx context.Context, jobID string) error
	Recover(ctx context.Context) (int, error)
}

// Store is what the pool needs from PostgreSQL. *database.WorkStore satisfies it.
type Store interface {
	Claim(ctx context.Context, id string) (bool, error)
	Complete(ctx context.Context, id string) error
	Fail(ctx context.Context, id string) error
	ResetProcessing(ctx context.Context) (int64, error)
}

// Handler does the work for one job. It must return when ctx is cancelled.
type Handler func(ctx context.Context, jobID string) error

// Pool runs Workers goroutines until its context ends.
type Pool struct {
	Queue  Queue
	Store  Store
	Handle Handler
	Logger *slog.Logger

	// Workers is how many jobs run at the same time.
	Workers int
	// DrainTimeout is how long active jobs may keep running after the pool
	// is told to stop. Keep it below the host's shutdown grace period, or the
	// host kills the process first and nothing gets recorded.
	DrainTimeout time.Duration
}

// Run recovers what a previous run left behind, starts the workers, and
// blocks until ctx ends and the workers have stopped.
//
// Shutdown has two separate signals:
//
//   - ctx (cancelled on SIGTERM) stops workers from TAKING new jobs.
//   - Active jobs run under their own context, which ctx does not cancel.
//     They get DrainTimeout to finish. Only after that is their context
//     cancelled; they then record nothing and keep their ID in the processing
//     list, so the next start runs them again.
//
// Run returns an error only if it can't start.
func (p *Pool) Run(ctx context.Context) error {
	if p.Workers < 1 {
		return fmt.Errorf("worker: pool needs at least 1 worker, got %d", p.Workers)
	}
	if err := p.recoverAtStartup(ctx); err != nil {
		return err
	}

	// WithoutCancel keeps ctx's values but not its cancellation: SIGTERM
	// doesn't reach running jobs. cancelJobs is called only after the drain
	// timeout (or when Run returns).
	jobsCtx, cancelJobs := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelJobs()

	var wg sync.WaitGroup
	for n := range p.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.work(ctx, jobsCtx, p.Logger.With("worker", n))
		}()
	}
	p.Logger.Info("worker pool started", "workers", p.Workers)

	<-ctx.Done()
	p.Logger.Info("worker pool stopping: no new jobs, waiting for active ones", "drain_timeout", p.DrainTimeout)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		p.Logger.Info("worker pool stopped: every active job finished")
		return nil
	case <-time.After(p.DrainTimeout):
	}

	p.Logger.Warn("worker pool: drain timeout reached; cancelling unfinished jobs, they run again after restart")
	cancelJobs()
	select {
	case <-done:
	case <-time.After(abandonWait):
		p.Logger.Error("worker pool: a job ignored cancellation; exiting without it")
	}
	return nil
}

// recoverAtStartup resets what a stopped process left half-done, before any
// worker claims anything:
//
//  1. PostgreSQL: jobs still 'processing' go back to 'queued', so Claim can
//     win them again.
//  2. Redis: IDs left in the processing list go back to the front of the
//     pending list, so a worker takes them first.
//
// SAFE ONLY WITH ONE WORKER PROCESS (one API instance). It assumes every
// processing job belongs to a process that is no longer running. A second
// process starting while the first works would reset jobs that are still
// running, and they would run twice at the same time. Several processes need
// leases instead (Phase 12).
func (p *Pool) recoverAtStartup(ctx context.Context) error {
	reset, err := p.Store.ResetProcessing(ctx)
	if err != nil {
		return err
	}
	requeued, err := p.Queue.Recover(ctx)
	if err != nil {
		return err
	}
	if reset > 0 || requeued > 0 {
		p.Logger.Info("worker pool: recovered unfinished jobs", "jobs_reset", reset, "ids_requeued", requeued)
	}
	return nil
}

// work is one worker's loop. stop ends the taking of new jobs; jobsCtx is
// what a taken job runs under.
func (p *Pool) work(stop, jobsCtx context.Context, log *slog.Logger) {
	for stop.Err() == nil {
		id, err := p.Queue.Take(stop, takeTimeout)
		switch {
		case err == nil:
			// Taken, so it's this worker's job now, even if the stop signal
			// arrived meanwhile: its ID is in the processing list.
			p.process(jobsCtx, log.With("job_id", id), id)
		case stop.Err() != nil:
			return
		case errors.Is(err, queue.ErrEmpty):
			// Nothing waiting; ask again.
		default:
			log.Warn("worker: can't reach the queue, retrying", "err", err)
			sleep(stop, time.Second)
		}
	}
}

// process runs one job. Errors are logged, never fatal: unless the outcome
// was saved, the ID stays in the processing list for the next start.
func (p *Pool) process(ctx context.Context, log *slog.Logger, id string) {
	won, err := p.Store.Claim(ctx, id)
	if err != nil {
		log.Warn("worker: claim failed; the ID stays in processing", "err", err)
		return
	}
	if !won {
		log.Info("worker: skipping, already taken, finished or deleted")
		p.ack(ctx, log, id)
		return
	}

	start := time.Now()
	err = p.Handle(ctx, id)
	if ctx.Err() != nil {
		// Only after the drain timeout. Record nothing: the job stays
		// processing and its ID stays in the processing list.
		log.Warn("worker: job cancelled at shutdown; it will run again after restart")
		return
	}

	saveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err != nil {
		log.Warn("worker: job failed", "err", err)
		if err := p.Store.Fail(saveCtx, id); err != nil {
			log.Warn("worker: couldn't record the failure; the ID stays in processing", "err", err)
			return
		}
	} else {
		if err := p.Store.Complete(saveCtx, id); err != nil {
			log.Warn("worker: couldn't record completion; the ID stays in processing", "err", err)
			return
		}
		log.Info("worker: job completed", "duration", time.Since(start).Round(time.Millisecond))
	}
	p.ack(saveCtx, log, id)
}

func (p *Pool) ack(ctx context.Context, log *slog.Logger, id string) {
	if err := p.Queue.Ack(ctx, id); err != nil {
		// Harmless: after a restart Recover puts it back and Claim skips it.
		log.Warn("worker: ack failed; the ID may be seen again and skipped", "err", err)
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
