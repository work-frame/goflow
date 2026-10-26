package job

import (
	"cmp"
	"errors"
	"slices"
	"testing"
	"time"
)

// sortedJobs returns n jobs, one minute apart, already in (CreatedAt, ID) order.
func sortedJobs(t *testing.T, n int) []*Job {
	t.Helper()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	out := make([]*Job, n)
	for i := range n {
		j, err := New("job")
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		j.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		j.UpdatedAt = j.CreatedAt
		out[i] = j
	}
	return out
}

// walk follows Next cursors until the last page and returns every job seen, in order.
func walk(t *testing.T, all []*Job, p ListParams) (seen []*Job, pages int) {
	t.Helper()
	for {
		page, err := Paginate(all, p)
		if err != nil {
			t.Fatalf("Paginate: %v", err)
		}
		pages++
		seen = append(seen, page.Jobs...)
		if page.Next == nil {
			return seen, pages
		}
		if pages > 100 {
			t.Fatal("pagination never reached a last page")
		}
		p.After = page.Next
	}
}

func TestPaginateVisitsEveryJobExactlyOnce(t *testing.T) {
	all := sortedJobs(t, 5)

	seen, pages := walk(t, all, ListParams{Limit: 2})

	if pages != 3 {
		t.Errorf("pages = %d, want 3 (2 + 2 + 1)", pages)
	}
	if len(seen) != len(all) {
		t.Fatalf("saw %d jobs, want %d", len(seen), len(all))
	}
	for i := range all {
		if seen[i].ID != all[i].ID {
			t.Errorf("position %d: got job %s, want %s", i, seen[i].ID, all[i].ID)
		}
	}
}

func TestPaginateExactMultipleHasNoEmptyLastPage(t *testing.T) {
	// 4 jobs at 2 per page is exactly 2 pages. Asking for limit+1 rows is what
	// lets page 2 know there is nothing after it, instead of sending an empty page 3.
	_, pages := walk(t, sortedJobs(t, 4), ListParams{Limit: 2})
	if pages != 2 {
		t.Errorf("pages = %d, want 2", pages)
	}
}

func TestPaginateBreaksTimestampTiesByID(t *testing.T) {
	all := sortedJobs(t, 3)
	for _, j := range all {
		j.CreatedAt = all[0].CreatedAt // all created at the same instant
	}
	slices.SortFunc(all, func(a, b *Job) int { return cmp.Compare(a.ID, b.ID) })

	seen, _ := walk(t, all, ListParams{Limit: 1})
	for i := range all {
		if seen[i].ID != all[i].ID {
			t.Errorf("position %d: got %s, want %s", i, seen[i].ID, all[i].ID)
		}
	}
}

func TestPaginateFiltersByStatus(t *testing.T) {
	all := sortedJobs(t, 5)
	all[1].Status = StatusCompleted
	all[3].Status = StatusCompleted

	seen, _ := walk(t, all, ListParams{Status: StatusCompleted, Limit: 1})
	if len(seen) != 2 || seen[0].ID != all[1].ID || seen[1].ID != all[3].ID {
		t.Errorf("got %d completed jobs, want jobs 1 and 3", len(seen))
	}
}

func TestPaginateRejectsBadLimits(t *testing.T) {
	for _, limit := range []int{-1, 0, MaxPageSize + 1} {
		if _, err := Paginate(nil, ListParams{Limit: limit}); !errors.Is(err, ErrInvalidLimit) {
			t.Errorf("limit %d: err = %v, want ErrInvalidLimit", limit, err)
		}
	}
}
