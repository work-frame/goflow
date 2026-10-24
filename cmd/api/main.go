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

	if err := run(logger); err != nil {
		logger.Error("server stopped with error", "err", err)
		os.Exit(1)
	}
}

// run starts the server and blocks until it stops.
// It returns an error instead of calling os.Exit, so deferred cleanup always runs.
func run(logger *slog.Logger) error {
	addr := os.Getenv("GOFLOW_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	// Bind the port first, so "listening" is only logged once it's true.
	// The error already names the address, so it isn't wrapped again.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	// In-memory only for Phase 2: jobs are lost on restart. Postgres arrives in Phase 3.
	store := job.NewMemoryStore()

	srv := &http.Server{
		Handler:           api.NewRouter(store),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Buffered so the goroutine can always send and exit, even if nobody is reading.
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()
	logger.Info("listening", "addr", ln.Addr().String())

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
