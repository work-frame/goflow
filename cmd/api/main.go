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
	"github.com/work-frame/goflow/internal/job"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	addr := os.Getenv("GOFLOW_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	// ctx is cancelled on Ctrl+C (SIGINT) or SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, logger, addr, nil)
	stop() // called directly, not deferred: os.Exit below would skip a defer

	if err != nil {
		logger.Error("server stopped with error", "err", err)
		os.Exit(1)
	}
}

// run serves the API on addr until ctx is cancelled, then shuts down gracefully.
// If ready is not nil, it is called with the bound address once the server is listening.
func run(ctx context.Context, logger *slog.Logger, addr string, ready func(addr string)) error {
	// Bind first, so "listening" is only logged once it's true.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	// In-memory only for Phase 2: jobs are lost on restart. Postgres arrives in Phase 3.
	store := job.NewMemoryStore()

	srv := &http.Server{
		Handler:           api.NewRouter(store, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	// Buffered so the goroutine can always send and exit, even if nobody is reading.
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

	// Stop accepting new connections and wait up to 10s for in-flight requests.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	// After Shutdown, Serve returns http.ErrServerClosed.
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	logger.Info("server stopped cleanly")
	return nil
}
