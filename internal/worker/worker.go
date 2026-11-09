// Package worker runs a pool of goroutines that take job IDs off the queue
// and run the jobs. Several pools, in several processes, can share one
// database and one Redis.
//
// Each job goes through the same steps, in an order that keeps it from being
// lost or half-recorded:
//
//	Take      ID moves pending → processing list (Redis)
//	Claim     the job becomes processing under a new lease (PostgreSQL);
//	          only one caller can win
//	Handle    the work itself, while a heartbeat renews the lease
//	Complete  or Retry or Bury, only with the lease's token (PostgreSQL)
//	Ack       ID leaves the processing list (Redis)
//
// A process that dies mid-job stops renewing its leases. Once they run out,
// the lease reaper (package relay) takes the jobs back and they run again:
// a job may run twice, but it is never silently dropped. A worker whose
// lease ran out can't record anything any more (see database.WorkStore).
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/work-frame/goflow/internal/database"
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
	// DefaultLease is how long a claim lasts without being renewed.
	DefaultLease = 30 * time.Second
)

// Queue is what the pool needs from Redis. *queue.Queue satisfies it.
type Queue interface {
	Take(ctx context.Context, timeout time.Duration) (string, error)
	Ack(ctx context.Context, jobID string) error
	Schedule(ctx context.Context, jobID string, at time.Time) error
	// Taken lists the processing list; Requeue moves one ID from it back
	// to the queue.
	Taken(ctx context.Context) ([]string, error)
	Requeue(ctx context.Context, jobID string) error
	// TrackLease and DropLease keep the lease reaper's list of deadlines.
	TrackLease(ctx context.Context, jobID string, until time.Time) error
	DropLease(ctx context.Context, jobID string) error
}

// Store is what the pool needs from PostgreSQL. *database.WorkStore
// satisfies it. Every write after Claim carries the lease token and fails
// with database.ErrLeaseLost once the lease isn't the job's any more.
type Store interface {
	Claim(ctx context.Context, id string, leaseFor time.Duration) (database.Claimed, bool, error)
	Renew(ctx context.Context, id, token string, leaseFor time.Duration) (time.Time, error)
	Complete(ctx context.Context, id, token string) error
	// Retry records a retry due after delay and returns when it is due.
	Retry(ctx context.Context, id, token string, delay time.Duration, lastError string) (time.Time, error)
	// Bury marks the job dead.
	Bury(ctx context.Context, id, token, lastError string) error
	// Processing reports which of ids are processing (someone's lease).
	Processing(ctx context.Context, ids []string) (map[string]bool, error)
}

// Run is one attempt at a job, as a Handler sees it.
type Run struct {
	JobID   string
	Title   string
	Attempt int // 1 for the first run
}

// Handler does the work for one attempt. ctx carries the job's deadline
// (Pool.JobTimeout) and is cancelled at shutdown. A handler must pass ctx to
// everything that can block (network calls, sleeps) and return when it ends:
// Go can't stop a goroutine from outside, so a handler that ignores ctx
// keeps its worker busy until it finishes on its own.
type Handler func(ctx context.Context, run Run) error

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
	// RetryWait is the first wait after a failed startup recovery (Redis or
	// the database not reachable yet). It doubles up to 30 s. Zero means 1 s.
	RetryWait time.Duration
	// Backoff returns how long a job waits before its next attempt, after
	// attempt n failed. Nil means the default, Backoff.
	Backoff func(attempt int) time.Duration
	// JobTimeout limits one attempt. A handler still running then gets its
	// context cancelled, and the attempt counts as failed ("timed out"), so
	// it is retried like any other failure. Zero means no limit.
	JobTimeout time.Duration
	// Observe, if set, hears how each attempt ended ("completed", "retry"
	// or "dead") and how long its handler ran, after the outcome is saved.
	// The API uses it for metrics; cmd/worker has no metrics endpoint.
	Observe func(outcome string, took time.Duration)
	// Lease is how long a claim lasts unless renewed. Zero means
	// DefaultLease (30 s).
	Lease time.Duration
	// HeartbeatEvery is how often a running job's lease is renewed. Zero
	// means a third of Lease, so two renewals can fail before it runs out.
	HeartbeatEvery time.Duration
}

func (p *Pool) lease() time.Duration {
	if p.Lease > 0 {
		return p.Lease
	}
	return DefaultLease
}

func (p *Pool) heartbeatEvery() time.Duration {
	if p.HeartbeatEvery > 0 {
		return p.HeartbeatEvery
	}
	return p.lease() / 3
}

// observe reports an attempt's outcome to Observe, if set.
func (p *Pool) observe(outcome string, took time.Duration) {
	if p.Observe != nil {
		p.Observe(outcome, took)
	}
}

// Run tidies what a stopped process left in the processing list, starts the
// workers, and blocks until ctx ends and the workers have stopped.
//
// Shutdown has two separate signals:
//
//   - ctx (cancelled on SIGTERM) stops workers from TAKING new jobs.
//   - Active jobs run under their own context, which ctx does not cancel.
//     They get DrainTimeout to finish, and their leases are renewed
//     meanwhile. Only after that is their context cancelled; they then
//     record nothing, their leases run out, and the reaper sends them to
//     another worker.
//
// Run returns an error only if it can't start.
func (p *Pool) Run(ctx context.Context) error {
	if p.Workers < 1 {
		return fmt.Errorf("worker: pool needs at least 1 worker, got %d", p.Workers)
	}
	// No worker starts before the cleanup has succeeded. If Redis or the
	// database isn't reachable yet (a fresh deploy can start before its Key
	// Value is ready), keep trying instead of giving up; jobs wait safely
	// meanwhile.
	wait := p.RetryWait
	if wait <= 0 {
		wait = time.Second
	}
	for {
		err := p.recoverAtStartup(ctx)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return nil // stopped before starting: nothing to drain
		}
		p.Logger.Warn("worker pool: startup recovery failed, retrying", "err", err, "retry_in", wait)
		sleep(ctx, wait)
		wait = min(wait*2, 30*time.Second)
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

	p.Logger.Warn("worker pool: drain timeout reached; cancelling unfinished jobs, they run again once their leases run out")
	cancelJobs()
	select {
	case <-done:
	case <-time.After(abandonWait):
		p.Logger.Error("worker pool: a job ignored cancellation; exiting without it")
	}
	return nil
}

// recoverAtStartup tidies the processing list before any worker of this
// pool takes anything. It never changes a job in PostgreSQL, so it can't
// disturb a job another process is running, however many are running.
//
// An ID in the processing list was taken by some worker, here or in
// another process. If its job is processing, someone holds its lease:
// the entry goes (the lease decides what happens to that job, and the
// reaper takes it back if its holder died). Otherwise the job was never
// claimed (its worker died between Take and Claim) or is already
// finished: the ID goes back to the queue. If a live worker had just taken
// it and is about to claim it, the copy only makes a duplicate, which
// Claim lets exactly one worker win.
func (p *Pool) recoverAtStartup(ctx context.Context) error {
	ids, err := p.Queue.Taken(ctx)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	leased, err := p.Store.Processing(ctx, ids)
	if err != nil {
		return err
	}
	var dropped, requeued int
	for _, id := range ids {
		if leased[id] {
			if err := p.Queue.Ack(ctx, id); err != nil {
				return err
			}
			dropped++
			continue
		}
		if err := p.Queue.Requeue(ctx, id); err != nil {
			return err
		}
		requeued++
	}
	p.Logger.Info("worker pool: tidied the processing list", "leased_elsewhere", dropped, "ids_requeued", requeued)
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

// errLeaseLost is the cause a job's context is cancelled with when its
// lease is gone.
var errLeaseLost = errors.New("lease lost")

// process runs one job. Errors are logged, never fatal: unless the outcome
// was saved, the job is taken back once its lease runs out.
func (p *Pool) process(ctx context.Context, log *slog.Logger, id string) {
	claimed, ok, err := p.Store.Claim(ctx, id, p.lease())
	if err != nil {
		log.Warn("worker: claim failed; the ID stays in processing", "err", err)
		return
	}
	if !ok {
		log.Info("worker: skipping, already taken, finished or deleted")
		p.ack(ctx, log, id)
		return
	}
	log = log.With("attempt", claimed.Attempt, "max_attempts", claimed.MaxAttempts)
	// Which process holds which job: docker compose logs prefixes each line
	// with the container. The deadline is PostgreSQL's; heartbeats move it.
	log.Info("worker: job claimed", "lease_expires", claimed.LeaseExpires)
	p.trackLease(ctx, log, id, claimed.LeaseExpires)

	// The handler runs under runCtx. The heartbeat cancels it, with
	// errLeaseLost as the cause, when the lease is gone.
	runCtx, cancelRun := context.WithCancelCause(ctx)
	defer cancelRun(nil)
	stopBeat := make(chan struct{})
	beatDone := make(chan struct{})
	go func() {
		defer close(beatDone)
		p.heartbeat(ctx, log, id, claimed.LeaseToken, stopBeat, cancelRun)
	}()

	start := time.Now()
	err = p.runHandler(runCtx, Run{JobID: id, Title: claimed.Title, Attempt: claimed.Attempt}, log)
	took := time.Since(start)
	close(stopBeat)
	<-beatDone

	if errors.Is(context.Cause(runCtx), errLeaseLost) {
		// Another worker may be running the job now. Whatever this attempt
		// did, it records nothing: that is the next holder's to do.
		log.Warn("worker: lease lost; stopped the job and recorded nothing")
		p.ack(ctx, log, id)
		return
	}
	if ctx.Err() != nil {
		// Only after the drain timeout. Record nothing: the lease runs out
		// and the reaper sends the job to another worker.
		log.Warn("worker: job cancelled at shutdown; it runs again once its lease runs out")
		return
	}

	saveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	token := claimed.LeaseToken
	switch {
	case err == nil:
		if err := p.Store.Complete(saveCtx, id, token); err != nil {
			p.notRecorded(saveCtx, log, id, "completion", err)
			return
		}
		log.Info("worker: job completed", "duration", took.Round(time.Millisecond))
		p.observe("completed", took)

	case IsPermanent(err) || claimed.Attempt >= claimed.MaxAttempts:
		reason := "out of attempts"
		if IsPermanent(err) {
			reason = "permanent error"
		}
		if err := p.Store.Bury(saveCtx, id, token, err.Error()); err != nil {
			p.notRecorded(saveCtx, log, id, "dead job", err)
			return
		}
		log.Warn("worker: job dead", "reason", reason, "err", err)
		p.observe("dead", took)

	default:
		if !p.retry(saveCtx, log, id, token, claimed.Attempt, err) {
			return
		}
		p.observe("retry", took)
	}
	p.dropLease(saveCtx, log, id)
	p.ack(saveCtx, log, id)
}

// heartbeat renews the lease every HeartbeatEvery until stop is closed.
// When the database says the lease is gone (ErrLeaseLost), or renewals have
// failed for a whole lease length (this worker can't know whether the job
// was taken back), it cancels the job with errLeaseLost and returns.
func (p *Pool) heartbeat(ctx context.Context, log *slog.Logger, id, token string, stop <-chan struct{}, cancel context.CancelCauseFunc) {
	every := p.heartbeatEvery()
	t := time.NewTicker(every)
	defer t.Stop()
	lastRenewed := time.Now() // the claim; a monotonic reading, so clock jumps don't matter
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
		}
		renewCtx, done := context.WithTimeout(ctx, every)
		until, err := p.Store.Renew(renewCtx, id, token, p.lease())
		done()
		switch {
		case err == nil:
			lastRenewed = time.Now()
			p.trackLease(ctx, log, id, until)
		case errors.Is(err, database.ErrLeaseLost):
			log.Warn("worker: lease lost; cancelling the job")
			cancel(errLeaseLost)
			return
		case time.Since(lastRenewed) >= p.lease():
			log.Warn("worker: can't renew the lease and it has run out; cancelling the job", "err", err)
			cancel(errLeaseLost)
			return
		default:
			log.Warn("worker: can't renew the lease; trying again", "err", err)
		}
	}
}

// notRecorded handles a failed write of the job's outcome. ErrLeaseLost
// means another worker owns the job now: this result is discarded for good,
// so the ID is acknowledged. Any other error leaves everything as it is:
// the lease runs out and the job runs again.
func (p *Pool) notRecorded(ctx context.Context, log *slog.Logger, id, what string, err error) {
	if errors.Is(err, database.ErrLeaseLost) {
		log.Warn("worker: lease lost before the "+what+" was recorded; discarded it", "err", err)
		p.ack(ctx, log, id)
		return
	}
	log.Warn("worker: couldn't record the "+what+"; the job runs again once its lease runs out", "err", err)
}

func (p *Pool) trackLease(ctx context.Context, log *slog.Logger, id string, until time.Time) {
	if err := p.Queue.TrackLease(ctx, id, until); err != nil {
		// PostgreSQL still has the lease; the reaper's hourly sweep finds it.
		log.Warn("worker: couldn't record the lease deadline in Redis", "err", err)
	}
}

func (p *Pool) dropLease(ctx context.Context, log *slog.Logger, id string) {
	if err := p.Queue.DropLease(ctx, id); err != nil {
		// Harmless: the reaper asks PostgreSQL, finds the job finished, and drops it.
		log.Warn("worker: couldn't drop the lease deadline from Redis", "err", err)
	}
}

// runHandler runs one attempt under the job timeout. Two different ends are
// kept apart: ctx (shutdown) ending means "record nothing", while the job's
// own deadline passing is an ordinary failure, retried with backoff.
func (p *Pool) runHandler(ctx context.Context, run Run, log *slog.Logger) error {
	if p.JobTimeout <= 0 {
		return p.Handle(ctx, run)
	}
	runCtx, cancel := context.WithTimeout(ctx, p.JobTimeout)
	defer cancel()

	err := p.Handle(runCtx, run)
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		if err == nil {
			// It finished, just late: its work is done, so it counts as done.
			// Treating it as failed would run it again.
			log.Warn("worker: job finished after its deadline", "timeout", p.JobTimeout)
			return nil
		}
		return fmt.Errorf("timed out after %v: %w", p.JobTimeout, err)
	}
	return err
}

// retry records a failed attempt that will run again, then schedules it.
// The order is the point: PostgreSQL first, because it is the source of
// truth; Redis only after that commit. If Redis then fails, the job is still
// 'retrying' with a due time in PostgreSQL, and the reconciler re-sends it
// once overdue. The other order could leave a retry scheduled in Redis that
// PostgreSQL never recorded. It reports whether the retry was recorded.
func (p *Pool) retry(ctx context.Context, log *slog.Logger, id, token string, attempt int, jobErr error) bool {
	backoff := p.Backoff
	if backoff == nil {
		backoff = Backoff
	}
	delay := backoff(attempt)
	due, err := p.Store.Retry(ctx, id, token, delay, jobErr.Error())
	if err != nil {
		p.notRecorded(ctx, log, id, "retry", err)
		return false
	}
	if err := p.Queue.Schedule(ctx, id, due); err != nil {
		log.Warn("worker: retry recorded but not scheduled; the reconciler will re-send it", "err", err)
	}
	log.Warn("worker: job failed, will retry", "err", jobErr, "retry_in", delay.Round(time.Millisecond))
	return true
}

func (p *Pool) ack(ctx context.Context, log *slog.Logger, id string) {
	if err := p.Queue.Ack(ctx, id); err != nil {
		// Harmless: a later start requeues it and Claim skips it.
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

// Placeholder returns a Handler that stands in for real work: it waits d,
// and gives up early if its context is cancelled. Real job types replace it
// later.
func Placeholder(d time.Duration) Handler {
	return func(ctx context.Context, _ Run) error {
		return sleepOrCancel(ctx, d)
	}
}

// sleepOrCancel waits d, or returns ctx's error if ctx ends first.
func sleepOrCancel(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Backoff is the default wait before the next attempt, after attempt n
// failed: 2 s, 4 s, 8 s, ... doubling up to 5 minutes, with "equal jitter":
// half of that is fixed and the other half random. The random half spreads
// out jobs that failed together, so they don't all retry in the same second;
// the fixed half keeps a retry from coming back immediately.
func Backoff(attempt int) time.Duration {
	const base, ceiling = 2 * time.Second, 5 * time.Minute
	d := base
	for i := 1; i < attempt && d < ceiling; i++ {
		d *= 2
	}
	d = min(d, ceiling)
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// permanentError marks a failure that retrying can't fix.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent wraps err so the worker marks the job dead at once instead of
// retrying it: an invalid email address will be just as invalid next time.
// Errors not wrapped are retried, the safer default: a needless retry costs
// a little, while calling a temporary failure permanent loses the job.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err: err}
}

// IsPermanent reports whether err, or any error it wraps, came from Permanent.
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}
