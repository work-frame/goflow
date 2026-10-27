package main

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// sessionCleanupInterval is how often the API deletes expired sessions.
// Expired sessions already can't log anyone in; this only stops the table
// growing forever, so once an hour is plenty.
const sessionCleanupInterval = time.Hour

// expiredSessionDeleter is what cleanup needs. *database.SessionStore satisfies it.
type expiredSessionDeleter interface {
	DeleteExpired(ctx context.Context) (int64, error)
}

// startSessionCleanup deletes expired sessions right away and then every
// interval, until ctx ends or stop is called. stop waits for a cleanup that is
// running to finish, so the database can be closed safely afterwards.
//
// Every API instance runs its own cleanup. That is safe: the DELETE is
// idempotent, and a second instance simply finds nothing left to delete.
func startSessionCleanup(ctx context.Context, logger *slog.Logger, sessions expiredSessionDeleter, interval time.Duration) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			deleteExpiredSessions(ctx, logger, sessions)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

func deleteExpiredSessions(ctx context.Context, logger *slog.Logger, sessions expiredSessionDeleter) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	n, err := sessions.DeleteExpired(ctx)
	switch {
	case errors.Is(err, context.Canceled):
		// Shutting down: not a failure.
	case err != nil:
		// The next tick tries again. Lookup ignores expired sessions anyway.
		logger.Warn("session cleanup failed", "err", err)
	case n > 0:
		logger.Info("expired sessions deleted", "count", n)
	}
}
