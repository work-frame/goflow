package database

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/work-frame/goflow/internal/job"
)

// deadJob adds a job, relays it (so its outbox row is gone, as in
// production), claims it and buries it.
func deadJob(t *testing.T, f *jobsFixture, title string) string {
	t.Helper()
	ctx := context.Background()
	id := addJobs(t, f, title)[0]
	if _, err := NewOutboxStore(f.db).Relay(ctx, 100, func(context.Context, []string) error { return nil }); err != nil {
		t.Fatalf("Relay: %v", err)
	}
	w := NewWorkStore(f.db)
	if !claim(t, w, id) {
		t.Fatal("Claim lost")
	}
	if err := w.Bury(ctx, id, leaseOf(t, f, id), "smtp: 550 mailbox unavailable"); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRerunGivesADeadJobFreshAttempts(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	ctx := context.Background()
	id := deadJob(t, f, "send email")

	j, err := f.s.Rerun(ctx, f.alice, id)
	if err != nil {
		t.Fatalf("Rerun: %v", err)
	}
	if j.Status != job.StatusPending || j.Attempts != 0 || j.NextAttemptAt != nil {
		t.Errorf("returned job: %s, %d attempts, next %v; want pending, 0, nil", j.Status, j.Attempts, j.NextAttemptAt)
	}
	status, attempts, next, lastError := jobRow(t, f, id)
	if status != "pending" || attempts != 0 || next != nil || lastError != "" {
		t.Errorf("stored: %s, %d attempts, next %v, last_error %q; want pending, 0, NULL, NULL", status, attempts, next, lastError)
	}

	// It travels like a new job: the outbox row is back, the relay sends
	// it, and the next claim is attempt 1.
	var sent []string
	if _, err := NewOutboxStore(f.db).Relay(ctx, 100, func(_ context.Context, ids []string) error {
		sent = ids
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0] != id {
		t.Fatalf("relay sent %v, want [%s]", sent, id)
	}
	c, _, err := NewWorkStore(f.db).Claim(ctx, id, testLease)
	if err != nil || c.Attempt != 1 {
		t.Fatalf("Claim after rerun = attempt %d, %v; want 1", c.Attempt, err)
	}
}

func TestRerunRefusesJobsThatArentDead(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	ctx := context.Background()
	w := NewWorkStore(f.db)
	ids := addJobs(t, f, "pending", "processing", "completed")
	claim(t, w, ids[1])
	claim(t, w, ids[2])
	if err := w.Complete(ctx, ids[2], leaseOf(t, f, ids[2])); err != nil {
		t.Fatal(err)
	}

	for _, id := range ids {
		before, _, _, _ := jobRow(t, f, id)
		if _, err := f.s.Rerun(ctx, f.alice, id); !errors.Is(err, job.ErrNotRerunnable) {
			t.Errorf("Rerun of a %s job = %v, want job.ErrNotRerunnable", before, err)
		}
		if after, _, _, _ := jobRow(t, f, id); after != before {
			t.Errorf("status changed from %s to %s", before, after)
		}
	}
}

func TestRerunIsScopedToTheOwner(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	ctx := context.Background()
	id := deadJob(t, f, "alice's")

	if _, err := f.s.Rerun(ctx, f.bob, id); !errors.Is(err, job.ErrNotFound) {
		t.Fatalf("bob's Rerun of alice's job = %v, want job.ErrNotFound", err)
	}
	if status, _, _, _ := jobRow(t, f, id); status != "dead" {
		t.Errorf("status = %s, want dead: bob changed alice's job", status)
	}
	if _, err := f.s.Rerun(ctx, f.alice, "cccccccccccccccccccccccccccccccc"); !errors.Is(err, job.ErrNotFound) {
		t.Errorf("Rerun of a missing job = %v, want job.ErrNotFound", err)
	}
}

// A double click on "retry": the requests reach PostgreSQL together. The
// row lock lets one win; the others wait, re-check the status (now
// pending), and change nothing. Without the status in the WHERE clause, a
// late second rerun could reset a job a worker had already claimed.
// Not parallel: its 10 callers use 10 connections at once.
func TestConcurrentRerunsHaveOneWinner(t *testing.T) {
	f := newJobsFixture(t)
	id := deadJob(t, f, "double click")
	const n = 10
	withConns(f.db, n) // all 10 reruns in flight together, or they don't race

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = f.s.Rerun(context.Background(), f.alice, id)
		}()
	}
	close(start)
	wg.Wait()

	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, job.ErrNotRerunnable):
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d Reruns won, want exactly 1", won)
	}
	if got := countRows(t, f, `SELECT count(*) FROM job_outbox WHERE job_id = $1`, id); got != 1 {
		t.Errorf("%d outbox rows, want 1", got)
	}
}
