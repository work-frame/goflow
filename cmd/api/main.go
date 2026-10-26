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
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	addr := os.Getenv("GOFLOW_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	dbURL := os.Getenv("GOFLOW_DATABASE_URL")

	// ctx is cancelled on Ctrl+C (SIGINT) or SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, logger, addr, dbURL, nil)
	stop() // called directly, not deferred: os.Exit below would skip a defer

	if err != nil {
		logger.Error("server stopped with error", "err", err)
		os.Exit(1)
	}
}

// run connects to the database, serves the API on addr until ctx is cancelled,
// then shuts down gracefully. If ready is not nil, it is called with the bound
// address once the server is listening.
func run(ctx context.Context, logger *slog.Logger, addr, dbURL string, ready func(addr string)) error {
	if dbURL == "" {
		return errors.New("GOFLOW_DATABASE_URL is required")
	}

	// Connect before opening the port: an instance that can't reach its database
	// should never start accepting traffic.
	db, err := database.Open(ctx, dbURL)
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

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	deps := api.Deps{
		Jobs:   database.NewJobStore(db),
		Users:  database.NewUserStore(db),
		Hasher: auth.Hasher{Cost: auth.DefaultCost},
		DB:     db,
		Logger: logger,
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
