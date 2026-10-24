package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/work-frame/goflow/internal/api"
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

	srv := &http.Server{
		Addr:              addr,
		Handler:           api.NewRouter(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	// ctx is cancelled when the process receives Ctrl+C (SIGINT) or SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Buffered so the goroutine can always send and exit, even if nobody is reading.
	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		// The server failed before any shutdown signal, e.g. port already in use.
		return fmt.Errorf("listen on %s: %w", addr, err)
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	// Stop accepting new connections and wait up to 10s for in-flight requests.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	// After Shutdown, ListenAndServe returns http.ErrServerClosed.
	// Reading it confirms the server goroutine has finished.
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	logger.Info("server stopped cleanly")
	return nil
}
