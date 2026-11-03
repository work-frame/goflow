package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/work-frame/goflow/internal/api"
	"github.com/work-frame/goflow/internal/auth"
	"github.com/work-frame/goflow/internal/database"
	"github.com/work-frame/goflow/internal/queue"
	"github.com/work-frame/goflow/internal/relay"
	"github.com/work-frame/goflow/internal/worker"
)

const (
	// relayRetry is the first wait after a failed relay pass; it doubles
	// after each further failure, up to 5 minutes.
	relayRetry = time.Second
	// reconcileEvery and staleAfter: once an hour, re-send jobs that have
	// been queued for more than 10 minutes (Redis may have lost their IDs).
	reconcileEvery = time.Hour
	staleAfter     = 10 * time.Minute
	// workerDrain is how long running jobs may finish after SIGTERM. It
	// must fit inside the host's shutdown delay (Render: 30 s by default)
	// together with the HTTP shutdown, which runs at the same time.
	workerDrain = 20 * time.Second
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		logger.Error("invalid configuration", "err", err)
		os.Exit(1)
	}

	// ctx is cancelled on Ctrl+C (SIGINT) or SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err = run(ctx, logger, cfg, nil)
	stop() // called directly, not deferred: os.Exit below would skip a defer

	if err != nil {
		logger.Error("server stopped with error", "err", err)
		os.Exit(1)
	}
}

// run connects to the database, serves the API on cfg.Addr until ctx is cancelled,
// then shuts down gracefully. If ready is not nil, it is called with the bound
// address once the server is listening. cfg is trusted: loadConfig validates it.
func run(ctx context.Context, logger *slog.Logger, cfg config, ready func(addr string)) error {
	// Never log cfg.DatabaseURL or cfg.RedisURL: they can contain passwords.
	logger.Info("starting", "addr", cfg.Addr, "bcrypt_cost", cfg.BcryptCost,
		"registration_open", cfg.RegistrationOpen, "queue_enabled", cfg.RedisURL != "", "workers", cfg.Workers)

	// Connect before opening the port: an instance that can't reach its database
	// should never start accepting traffic.
	db, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	// Deferred, so it runs after Shutdown below: stop HTTP first, then close the pool.
	defer db.Close()
	logger.Info("database connected")

	applied, err := database.Migrate(ctx, db)
	if err != nil {
		return err
	}
	if len(applied) > 0 {
		logger.Info("migrations applied", "versions", applied)
	} else {
		logger.Info("database schema up to date")
	}

	// The queue is optional until workers exist (Phase 6). Without it, jobs
	// and their outbox rows are saved and wait until a relay runs.
	// rl stays a nil *relay.Relay when GOFLOW_REDIS_URL is unset.
	var rl *relay.Relay
	if cfg.RedisURL != "" {
		queue.UseLogger(logger)
		// New doesn't connect. An unreachable Redis must not stop the API:
		// jobs are saved to the outbox either way, and the relay, the
		// reconciler and the worker pool all retry until Redis answers. On
		// Render, a Blueprint sync that creates the Key Value and redeploys
		// the API at once can start the API first (Task 27's failed deploy).
		q, err := queue.New(cfg.RedisURL)
		if err != nil {
			return err
		}
		defer q.Close()
		pingCtx, cancelPing := context.WithTimeout(ctx, 3*time.Second)
		if err := q.PingContext(pingCtx); err != nil {
			logger.Warn("queue not reachable yet; jobs wait in the outbox and the relay and workers keep retrying", "err", err)
		} else {
			logger.Info("queue connected")
		}
		cancelPing()

		outbox := database.NewOutboxStore(db)
		rl = relay.New(outbox, q, logger, relayRetry)
		// Deferred after q.Close and db.Close, so these run first: the relay
		// and the reconciler stop, then the queue and the database close.
		stopRelay := rl.Start(ctx)
		defer stopRelay()
		stopReconciler := relay.NewReconciler(outbox, q, logger, reconcileEvery, staleAfter).Start(ctx)
		defer stopReconciler()

		if cfg.Workers > 0 {
			// The pool's startup recovery resets every 'processing' job, so
			// ONLY ONE API INSTANCE may run workers (see worker.Pool.Run).
			pool := &worker.Pool{
				Queue:        q,
				Store:        database.NewWorkStore(db),
				Handle:       worker.Placeholder(cfg.SimulatedWork),
				Logger:       logger,
				Workers:      cfg.Workers,
				DrainTimeout: workerDrain,
			}
			// The pool stops taking jobs when ctx ends (SIGTERM), at the same
			// moment the HTTP server starts shutting down, and drains in
			// parallel with it. stopPool also covers run returning early
			// (a serve error) without SIGTERM. Deferred after q.Close and
			// db.Close, so the pool has finished before they close.
			poolCtx, stopPool := context.WithCancel(ctx)
			poolDone := make(chan struct{})
			go func() {
				defer close(poolDone)
				if err := pool.Run(poolCtx); err != nil {
					logger.Error("worker pool could not start; jobs wait in Redis", "err", err)
				}
			}()
			defer func() {
				stopPool()
				<-poolDone
			}()
		}
	} else {
		logger.Warn("queue disabled: GOFLOW_REDIS_URL is not set, so new jobs wait in the outbox")
	}

	sessions := database.NewSessionStore(db)
	// Deferred after db.Close, so it runs first: cleanup stops, then the pool closes.
	stopCleanup := startSessionCleanup(ctx, logger, sessions, sessionCleanupInterval)
	defer stopCleanup()

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}

	deps := api.Deps{
		Jobs:     database.NewJobStore(db),
		Users:    database.NewUserStore(db),
		Sessions: sessions,
		Hasher:   auth.Hasher{Cost: cfg.BcryptCost},
		DB:       db,
		Logger:   logger,

		RegistrationClosed: !cfg.RegistrationOpen,
	}
	// Only a real relay goes into the interface. Assigning a nil *relay.Relay
	// would make deps.Relay != nil (an interface holding a nil pointer is not
	// nil), and the first job would call Wake on nil and panic (case 25).
	if rl != nil {
		deps.Relay = rl
	}

	srv := &http.Server{
		Handler:           api.NewRouter(deps),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()

	boundAddr := ln.Addr().String()
	logger.Info("listening", "addr", boundAddr)
	if ready != nil {
		ready(boundAddr)
	}

	select {
	case err := <-errCh:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	logger.Info("server stopped cleanly")
	return nil
}
