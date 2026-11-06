// Command worker runs a pool of workers that take job IDs off the Redis
// queue and run the jobs.
//
// It needs the same database as the API and the Redis the API's relay
// pushes to. It doesn't run migrations: the API owns the schema, so start
// the API (or run it once) before the worker.
//
// Any number of worker processes (and API instances with workers) can run
// at once: each job is held under a lease, and a process only takes back
// jobs whose lease has run out (see worker.Pool and database.WorkStore).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/work-frame/goflow/internal/database"
	"github.com/work-frame/goflow/internal/events"
	"github.com/work-frame/goflow/internal/queue"
	"github.com/work-frame/goflow/internal/relay"
	"github.com/work-frame/goflow/internal/urlcheck"
	"github.com/work-frame/goflow/internal/worker"
)

const (
	// simulatedWork is how long the placeholder handler takes. Long enough to
	// stop the worker mid-job by hand; real job types replace it later.
	simulatedWork = 5 * time.Second
	// drainTimeout is how long running jobs may finish after SIGTERM.
	drainTimeout = 20 * time.Second
	// jobTimeout limits one attempt; a job still running then is retried.
	jobTimeout = 15 * time.Second
	// Allowed range for GOFLOW_WORKERS.
	defaultWorkers, maxWorkers = 4, 32
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	dbURL, redisURL := os.Getenv("GOFLOW_DATABASE_URL"), os.Getenv("GOFLOW_REDIS_URL")
	workers, err := workerCount(os.Getenv("GOFLOW_WORKERS"))
	if err = errors.Join(err, checkSettings(dbURL, redisURL)); err != nil {
		logger.Error("invalid configuration", "err", err)
		os.Exit(1)
	}

	// ctx is cancelled on Ctrl+C or SIGTERM. For the pool that means "take no
	// new jobs"; running jobs keep their own context (see worker.Pool.Run).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err = run(ctx, logger, dbURL, redisURL, workers)
	stop()
	if err != nil {
		logger.Error("worker stopped with error", "err", err)
		os.Exit(1)
	}
}

// checkSettings applies the API's rules: both URLs required, and TLS for
// anything on another machine. Errors never include the URLs.
func checkSettings(dbURL, redisURL string) error {
	var errs []error
	if dbURL == "" {
		errs = append(errs, errors.New("GOFLOW_DATABASE_URL is required"))
	} else if err := urlcheck.DatabaseTLS(dbURL); err != nil {
		errs = append(errs, err)
	}
	if redisURL == "" {
		errs = append(errs, errors.New("GOFLOW_REDIS_URL is required: the worker reads jobs from Redis"))
	} else if err := urlcheck.RedisTLS(redisURL); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// workerCount reads GOFLOW_WORKERS: empty means the default.
func workerCount(raw string) (int, error) {
	if raw == "" {
		return defaultWorkers, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxWorkers {
		return 0, fmt.Errorf("GOFLOW_WORKERS must be a whole number from 1 to %d, got %q", maxWorkers, raw)
	}
	return n, nil
}

func run(ctx context.Context, logger *slog.Logger, dbURL, redisURL string, workers int) error {
	db, err := database.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer db.Close()

	queue.UseLogger(logger)
	q, err := queue.Open(ctx, redisURL)
	if err != nil {
		return err
	}
	defer q.Close()

	// Each status change is published for live updates. The publisher runs
	// past SIGTERM (WithoutCancel) until its deferred stop, after the pool
	// has drained, so finishing jobs still announce it.
	pub := events.NewPublisher(q, logger)
	stopPublisher := pub.Start(context.WithoutCancel(ctx))
	defer stopPublisher()

	// Due retries move from Redis's delayed set to the queue.
	stopPromoter := relay.NewPromoter(q, logger, time.Second).Start(ctx)
	defer stopPromoter()
	// Jobs whose worker stopped renewing its lease go back to the queue.
	// Every process runs a reaper; PostgreSQL lets one take each job back.
	work := database.NewWorkStore(db).WithNotifier(pub)
	stopReaper := relay.NewLeaseReaper(work, q, logger, 5*time.Second, time.Hour).Start(ctx)
	defer stopReaper()

	logger.Info("worker starting", "workers", workers, "simulated_work", simulatedWork, "drain_timeout", drainTimeout)
	p := &worker.Pool{
		Queue:        q,
		Store:        work,
		Handle:       worker.Demo(simulatedWork),
		JobTimeout:   jobTimeout,
		Logger:       logger,
		Workers:      workers,
		DrainTimeout: drainTimeout,
	}
	err = p.Run(ctx)
	logger.Info("worker stopped")
	return err
}
