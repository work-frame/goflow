package database

import (
	"context"
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

// A worker that died mid-job left it processing; claiming it again resumes it.
func TestClaimResumesAProcessingJob(t *testing.T) {
	f := newJobsFixture(t)
	w := NewWorkStore(f.db)
	id := addJobs(t, f, "interrupted")[0]

	claim(t, w, id)
	if !claim(t, w, id) {
		t.Fatal("second Claim of a processing job = false, want true")
	}
}
