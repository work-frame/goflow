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
		"registration_open", cfg.RegistrationOpen, "queue_enabled", cfg.RedisURL != "")

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

	// The queue is optional until workers exist (Phase 6). q stays a nil
	// *queue.Queue when GOFLOW_REDIS_URL is unset.
	var q *queue.Queue
	if cfg.RedisURL != "" {
		q, err = queue.Open(ctx, cfg.RedisURL)
		if err != nil {
			return err
		}
		defer q.Close()
		logger.Info("queue connected")
	} else {
		logger.Warn("queue disabled: GOFLOW_REDIS_URL is not set, so new jobs are saved but not queued")
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
	// Only a real queue goes into the interface. Assigning a nil *queue.Queue
	// would make deps.Queue != nil (an interface holding a nil pointer is not
	// nil), and the first job would call Enqueue on nil and panic.
	if q != nil {
		deps.Queue = q
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
