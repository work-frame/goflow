package database

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/work-frame/goflow/internal/job"
)

// newTestJobStore returns a JobStore on a fresh, migrated, throwaway database.
func newTestJobStore(t *testing.T) *JobStore {
	t.Helper()
	db := newMigratedTestDB(t)
	return NewJobStore(db)
}

func mustNewJob(t *testing.T, title string) *job.Job {
	t.Helper()
	j, err := job.New(title)
	if err != nil {
		t.Fatalf("job.New(%q): %v", title, err)
	}
	return j
}

func titles(jobs []*job.Job) []string {
	out := make([]string, len(jobs))
	for i, j := range jobs {
		out[i] = j.Title
	}
	return out
}

func TestJobStoreAddAndGetRoundTrip(t *testing.T) {
	s := newTestJobStore(t)
	ctx := context.Background()
	want := mustNewJob(t, "send welcome email")

	if err := s.Add(ctx, want); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, err := s.Get(ctx, want.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.ID != want.ID || got.Title != want.Title || got.Status != want.Status {
		t.Errorf("got %+v, want %+v", got, want)
	}
	// Equal compares instants. == would also compare time zones and Go's monotonic clock reading.
	if !got.CreatedAt.Equal(want.CreatedAt) || !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("timestamps changed on round trip:\n got  %v\n want %v", got.CreatedAt, want.CreatedAt)
	}
	if got.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt location = %v, want UTC", got.CreatedAt.Location())
	}
}

func TestJobStoreGetMissing(t *testing.T) {
	s := newTestJobStore(t)

	_, err := s.Get(context.Background(), "does-not-exist")
	if !errors.Is(err, job.ErrNotFound) {
		t.Errorf("err = %v, want job.ErrNotFound", err)
	}
}

func TestJobStoreAddRejectsBadInput(t *testing.T) {
	s := newTestJobStore(t)
	ctx := context.Background()
	j := mustNewJob(t, "send email")

	if err := s.Add(ctx, j); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if err := s.Add(ctx, j); !errors.Is(err, job.ErrDuplicateID) {
		t.Errorf("second Add err = %v, want job.ErrDuplicateID", err)
	}
	if err := s.Add(ctx, nil); !errors.Is(err, job.ErrNilJob) {
		t.Errorf("Add(nil) err = %v, want job.ErrNilJob", err)
	}
}

func TestJobStoreList(t *testing.T) {
	s := newTestJobStore(t)
	ctx := context.Background()

	empty, err := s.List(ctx, "")
	if err != nil {
		t.Fatalf("List on empty table: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Errorf("List on empty table = %v, want an empty non-nil slice", empty)
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	inputs := []struct {
		title  string
		offset time.Duration
		status job.Status
	}{
		// Inserted out of order on purpose.
		{"third", 2 * time.Minute, job.StatusPending},
		{"first", 0, job.StatusCompleted},
		{"second", time.Minute, job.StatusPending},
	}
	for _, in := range inputs {
		j := mustNewJob(t, in.title)
		j.CreatedAt = base.Add(in.offset)
		j.UpdatedAt = j.CreatedAt
		j.Status = in.status
		if err := s.Add(ctx, j); err != nil {
			t.Fatalf("Add(%q): %v", in.title, err)
		}
	}

	all, err := s.List(ctx, "")
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if got, want := titles(all), []string{"first", "second", "third"}; !slices.Equal(got, want) {
		t.Errorf("List all = %v, want %v", got, want)
	}

	pending, err := s.List(ctx, job.StatusPending)
	if err != nil {
		t.Fatalf("List pending: %v", err)
	}
	if got, want := titles(pending), []string{"second", "third"}; !slices.Equal(got, want) {
		t.Errorf("List pending = %v, want %v", got, want)
	}
}

func TestJobStoreDelete(t *testing.T) {
	s := newTestJobStore(t)
	ctx := context.Background()
	j := mustNewJob(t, "send email")

	if err := s.Add(ctx, j); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Delete(ctx, j.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, j.ID); !errors.Is(err, job.ErrNotFound) {
		t.Errorf("Get after Delete err = %v, want job.ErrNotFound", err)
	}
	if err := s.Delete(ctx, j.ID); !errors.Is(err, job.ErrNotFound) {
		t.Errorf("second Delete err = %v, want job.ErrNotFound", err)
	}
}

func TestJobStoreStopsWhenContextIsCancelled(t *testing.T) {
	s := newTestJobStore(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled, like a client that disconnected

	_, err := s.Get(ctx, "anything")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestJobStoreConcurrentAdds(t *testing.T) {
	s := newTestJobStore(t)
	ctx := context.Background()
	const n = 50

	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Not mustNewJob: t.Fatalf must never be called from a goroutine the test started.
			j, err := job.New(fmt.Sprintf("job %d", i))
			if err != nil {
				t.Errorf("job.New: %v", err)
				return
			}
			if err := s.Add(ctx, j); err != nil {
				t.Errorf("Add: %v", err)
			}
		}()
	}
	wg.Wait()

	all, err := s.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != n {
		t.Errorf("stored %d jobs, want %d", len(all), n)
	}
}
