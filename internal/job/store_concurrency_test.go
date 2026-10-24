package job

import (
	"fmt"
	"sync"
	"testing"
)

func TestMemoryStoreConcurrentAccess(t *testing.T) {
	s := NewMemoryStore()
	const workers = 100

	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()

			j, err := New(fmt.Sprintf("job %d", i))
			if err != nil {
				t.Errorf("New: %v", err)
				return
			}
			if err := s.Add(j); err != nil {
				t.Errorf("Add: %v", err)
				return
			}
			if _, err := s.Get(j.ID); err != nil {
				t.Errorf("Get: %v", err)
			}
			_ = s.List()
		}()
	}
	wg.Wait()

	if n := len(s.List()); n != workers {
		t.Errorf("got %d jobs, want %d", n, workers)
	}
}

func TestMemoryStoreReturnsCopies(t *testing.T) {
	s := NewMemoryStore()
	j := mustNew(t, "original")
	if err := s.Add(j); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Changing the caller's job after Add must not change the stored one.
	j.Title = "changed after Add"
	got, err := s.Get(j.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "original" {
		t.Errorf("after Add: stored title = %q, want original", got.Title)
	}

	// Changing a job returned by Get must not change the stored one.
	got.Title = "changed after Get"
	again, _ := s.Get(j.ID)
	if again.Title != "original" {
		t.Errorf("after Get: stored title = %q, want original", again.Title)
	}

	// Same for List.
	s.List()[0].Title = "changed after List"
	again, _ = s.Get(j.ID)
	if again.Title != "original" {
		t.Errorf("after List: stored title = %q, want original", again.Title)
	}
}
