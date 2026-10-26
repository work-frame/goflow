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

// newTestJobStore returns a JobStore on a private copy of the migrated template.
func newTestJobStore(t *testing.T) *JobStore {
	t.Helper()
	return NewJobStore(newMigratedTestDB(t))
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

func ids(jobs []*job.Job) []string {
	out := make([]string, len(jobs))
	for i, j := range jobs {
		out[i] = j.ID
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

	empty, err := s.List(ctx, job.ListParams{Limit: 10})
	if err != nil {
		t.Fatalf("List on empty table: %v", err)
	}
	if empty.Jobs == nil || len(empty.Jobs) != 0 || empty.Next != nil {
		t.Errorf("List on empty table = %+v, want an empty non-nil slice and no cursor", empty)
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	inputs := []struct {
		title  string
		offset time.Duration
		status job.Status
	}{
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

	all, err := s.List(ctx, job.ListParams{Limit: 10})
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if got, want := titles(all.Jobs), []string{"first", "second", "third"}; !slices.Equal(got, want) {
		t.Errorf("List all = %v, want %v", got, want)
	}

	pending, err := s.List(ctx, job.ListParams{Status: job.StatusPending, Limit: 10})
	if err != nil {
		t.Fatalf("List pending: %v", err)
	}
	if got, want := titles(pending.Jobs), []string{"second", "third"}; !slices.Equal(got, want) {
		t.Errorf("List pending = %v, want %v", got, want)
	}
}

func TestJobStoreListPaginates(t *testing.T) {
	s := newTestJobStore(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Five jobs. The last two share a created_at, so the ID has to break the tie.
	offsets := []time.Duration{0, time.Minute, 2 * time.Minute, 3 * time.Minute, 3 * time.Minute}
	for i, off := range offsets {
		j := mustNewJob(t, fmt.Sprintf("job %d", i))
		j.CreatedAt = base.Add(off)
		j.UpdatedAt = j.CreatedAt
		if err := s.Add(ctx, j); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	everything, err := s.List(ctx, job.ListParams{Limit: job.MaxPageSize})
	if err != nil {
		t.Fatalf("List everything: %v", err)
	}
	want := ids(everything.Jobs)
	if len(want) != len(offsets) {
		t.Fatalf("stored %d jobs, want %d", len(want), len(offsets))
	}

	var got []string
	var after *job.Cursor
	for pages := 1; ; pages++ {
		page, err := s.List(ctx, job.ListParams{After: after, Limit: 2})
		if err != nil {
			t.Fatalf("List page %d: %v", pages, err)
		}
		got = append(got, ids(page.Jobs)...)
		if page.Next == nil {
			if pages != 3 {
				t.Errorf("pages = %d, want 3 (2 + 2 + 1)", pages)
			}
			break
		}
		if pages > 100 {
			t.Fatal("pagination never reached a last page")
		}
		after = page.Next
	}

	if !slices.Equal(got, want) {
		t.Errorf("paged IDs = %v,\nwant %v", got, want)
	}
}

func TestJobStoreListRejectsBadLimit(t *testing.T) {
	s := newTestJobStore(t)

	_, err := s.List(context.Background(), job.ListParams{Limit: 0})
	if !errors.Is(err, job.ErrInvalidLimit) {
		t.Errorf("err = %v, want job.ErrInvalidLimit", err)
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
	cancel()

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

	all, err := s.List(ctx, job.ListParams{Limit: job.MaxPageSize})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all.Jobs) != n {
		t.Errorf("stored %d jobs, want %d", len(all.Jobs), n)
	}
}
