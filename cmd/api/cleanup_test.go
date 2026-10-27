package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// countingDeleter counts calls and can fail on purpose.
type countingDeleter struct {
	calls atomic.Int64
	err   error
}

func (d *countingDeleter) DeleteExpired(ctx context.Context) (int64, error) {
	d.calls.Add(1)
	return 0, d.err
}

func waitForCalls(t *testing.T, d *countingDeleter, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for d.calls.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("cleanup ran %d times in 5s, want at least %d", d.calls.Load(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSessionCleanupRunsAtStartAndOnEveryTick(t *testing.T) {
	d := &countingDeleter{}
	stop := startSessionCleanup(context.Background(), discardLogger(), d, 10*time.Millisecond)
	defer stop()

	waitForCalls(t, d, 3) // once at start, then on two ticks
}

func TestSessionCleanupStopsAndStaysStopped(t *testing.T) {
	d := &countingDeleter{}
	stop := startSessionCleanup(context.Background(), discardLogger(), d, 5*time.Millisecond)
	waitForCalls(t, d, 1)

	stop() // returns only after the goroutine has exited
	after := d.calls.Load()
	time.Sleep(30 * time.Millisecond)
	if got := d.calls.Load(); got != after {
		t.Errorf("cleanup ran %d more times after stop", got-after)
	}
}

func TestSessionCleanupStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stop := startSessionCleanup(ctx, discardLogger(), &countingDeleter{}, time.Hour)
	cancel()

	finished := make(chan struct{})
	go func() { stop(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup goroutine did not exit after its context ended")
	}
}

// A failing cleanup is logged and retried on the next tick, never fatal.
func TestSessionCleanupKeepsGoingAfterErrors(t *testing.T) {
	d := &countingDeleter{err: errors.New("database is down")}
	stop := startSessionCleanup(context.Background(), discardLogger(), d, 5*time.Millisecond)
	defer stop()

	waitForCalls(t, d, 3)
}
