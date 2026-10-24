package job

import (
	"errors"
	"testing"
	"time"
)

// mustNew creates a job or fails the test immediately.
func mustNew(t *testing.T, title string) *Job {
	t.Helper()
	j, err := New(title)
	if err != nil {
		t.Fatalf("New(%q): %v", title, err)
	}
	return j
}

func TestMemoryStoreAddAndGet(t *testing.T) {
	s := NewMemoryStore()
	j := mustNew(t, "send email")

	if err := s.Add(j); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := s.Get(j.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != j.ID || got.Title != j.Title {
		t.Errorf("Get returned %+v, want %+v", got, j)
	}
}

func TestMemoryStoreGetMissing(t *testing.T) {
	s := NewMemoryStore()

	_, err := s.Get("does-not-exist")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreAddRejectsBadInput(t *testing.T) {
	s := NewMemoryStore()
	j := mustNew(t, "send email")

	if err := s.Add(j); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if err := s.Add(j); !errors.Is(err, ErrDuplicateID) {
		t.Errorf("second Add error = %v, want ErrDuplicateID", err)
	}
	if err := s.Add(nil); !errors.Is(err, ErrNilJob) {
		t.Errorf("Add(nil) error = %v, want ErrNilJob", err)
	}
}

func TestMemoryStoreDelete(t *testing.T) {
	s := NewMemoryStore()
	j := mustNew(t, "send email")

	if err := s.Add(j); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Delete(j.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(j.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete error = %v, want ErrNotFound", err)
	}
	if err := s.Delete(j.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete error = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreListSortedByCreatedAt(t *testing.T) {
	s := NewMemoryStore()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Added out of order on purpose.
	inputs := []struct {
		title  string
		offset time.Duration
	}{
		{"third", 2 * time.Minute},
		{"first", 0},
		{"second", time.Minute},
	}
	for _, in := range inputs {
		j := mustNew(t, in.title)
		j.CreatedAt = base.Add(in.offset)
		if err := s.Add(j); err != nil {
			t.Fatalf("Add(%q): %v", in.title, err)
		}
	}

	got := s.List()
	want := []string{"first", "second", "third"}
	if len(got) != len(want) {
		t.Fatalf("len(List()) = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Title != want[i] {
			t.Errorf("List()[%d].Title = %q, want %q", i, got[i].Title, want[i])
		}
	}
}

func TestMemoryStoreListEmptyIsNotNil(t *testing.T) {
	got := NewMemoryStore().List()
	if got == nil {
		t.Error("List() returned nil, want empty slice")
	}
	if len(got) != 0 {
		t.Errorf("len(List()) = %d, want 0", len(got))
	}
}
