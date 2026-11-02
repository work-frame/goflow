// Command worker takes job IDs off the Redis queue and runs the jobs.
//
// It needs the same database as the API and the Redis the API's relay
// pushes to. It doesn't run migrations: the API owns the schema, so start
// the API (or run it once) before the worker.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/work-frame/goflow/internal/database"
	"github.com/work-frame/goflow/internal/queue"
	"github.com/work-frame/goflow/internal/urlcheck"
	"github.com/work-frame/goflow/internal/worker"
)

// simulatedWork is how long the placeholder handler takes. Long enough to
// stop the worker mid-job by hand; real job types replace it later.
const simulatedWork = 5 * time.Second

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	dbURL, redisURL := os.Getenv("GOFLOW_DATABASE_URL"), os.Getenv("GOFLOW_REDIS_URL")
	if err := checkSettings(dbURL, redisURL); err != nil {
		logger.Error("invalid configuration", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, logger, dbURL, redisURL)
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

func run(ctx context.Context, logger *slog.Logger, dbURL, redisURL string) error {
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

	logger.Info("worker started", "simulated_work", simulatedWork)
	w := &worker.Worker{
		Queue:  q,
		Store:  database.NewWorkStore(db),
		Handle: placeholder,
		Logger: logger,
	}
	err = w.Run(ctx)
	logger.Info("worker stopped")
	return err
}

// placeholder stands in for real work: it waits, and gives up early if the
// worker is told to stop.
func placeholder(ctx context.Context, jobID string) error {
	t := time.NewTimer(simulatedWork)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
