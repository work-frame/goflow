package database

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/work-frame/goflow/internal/job"
)

func claim(t *testing.T, w *WorkStore, id string) bool {
	t.Helper()
	ok, err := w.Claim(context.Background(), id)
	if err != nil {
		t.Fatalf("Claim(%s): %v", id, err)
	}
	return ok
}

func TestClaimThenComplete(t *testing.T) {
	f := newJobsFixture(t)
	w := NewWorkStore(f.db)
	id := addJobs(t, f, "do it")[0]

	if !claim(t, w, id) {
		t.Fatal("Claim of a pending job = false, want true")
	}
	if s := statusOf(t, f, id); s != job.StatusProcessing {
		t.Fatalf("status after Claim = %s, want processing", s)
	}
	if err := w.Complete(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if s := statusOf(t, f, id); s != job.StatusCompleted {
		t.Fatalf("status after Complete = %s, want completed", s)
	}
}

// The second copy of a duplicate ID finds the job finished and skips it.
func TestClaimSkipsFinishedAndMissingJobs(t *testing.T) {
	f := newJobsFixture(t)
	w := NewWorkStore(f.db)
	ctx := context.Background()
	ids := addJobs(t, f, "done", "broken")

	for i, finish := range []func(context.Context, string) error{w.Complete, w.Fail} {
		claim(t, w, ids[i])
		if err := finish(ctx, ids[i]); err != nil {
			t.Fatal(err)
		}
		if claim(t, w, ids[i]) {
			t.Errorf("Claim of a finished job (%s) = true, want false", statusOf(t, f, ids[i]))
		}
	}
	if claim(t, w, "cccccccccccccccccccccccccccccccc") {
		t.Error("Claim of a job that doesn't exist = true, want false")
	}
}

// A processing job is someone else's: claiming it again must lose.
func TestClaimRefusesAProcessingJob(t *testing.T) {
	f := newJobsFixture(t)
	w := NewWorkStore(f.db)
	id := addJobs(t, f, "taken")[0]

	if !claim(t, w, id) {
		t.Fatal("first Claim = false, want true")
	}
	if claim(t, w, id) {
		t.Fatal("second Claim of a processing job = true, want false: the job would run twice")
	}
}

// Two goroutines claim the same job at the same moment, over and over,
// against real PostgreSQL. Exactly one may win each time. The goroutines use
// separate pooled connections, so this is the same race as two processes.
func TestClaimHasExactlyOneWinner(t *testing.T) {
	f := newJobsFixture(t)
	w := NewWorkStore(f.db)
	ctx := context.Background()
	const rounds = 50

	ids := make([]string, rounds)
	for i := range rounds {
		ids[i] = addJobs(t, f, fmt.Sprintf("race %d", i))[0]
	}

	for i, id := range ids {
		start := make(chan struct{})
		results := make(chan bool, 2)
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start // both goroutines are ready; release them together
				won, err := w.Claim(ctx, id)
				if err != nil {
					errs <- err
					return
				}
				results <- won
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		close(errs)
		for err := range errs {
			t.Fatalf("round %d: Claim: %v", i, err)
		}
		winners := 0
		for won := range results {
			if won {
				winners++
			}
		}
		if winners != 1 {
			t.Fatalf("round %d: %d goroutines won the claim, want exactly 1", i, winners)
		}
	}
}

// Startup recovery: processing jobs go back to queued; other statuses stay.
func TestResetProcessing(t *testing.T) {
	f := newJobsFixture(t)
	w := NewWorkStore(f.db)
	ctx := context.Background()
	ids := addJobs(t, f, "stuck one", "stuck two", "finished", "waiting")

	for _, id := range ids[:3] {
		claim(t, w, id)
	}
	if err := w.Complete(ctx, ids[2]); err != nil {
		t.Fatal(err)
	}

	n, err := w.ResetProcessing(ctx)
	if err != nil {
		t.Fatalf("ResetProcessing: %v", err)
	}
	if n != 2 {
		t.Fatalf("ResetProcessing changed %d jobs, want 2", n)
	}
	want := map[string]job.Status{ids[0]: job.StatusQueued, ids[1]: job.StatusQueued, ids[2]: job.StatusCompleted, ids[3]: job.StatusPending}
	for id, status := range want {
		if got := statusOf(t, f, id); got != status {
			t.Errorf("job %s: status %s, want %s", id, got, status)
		}
	}
	if !claim(t, w, ids[0]) {
		t.Error("a reset job can't be claimed again")
	}
}
