package job

import (
	"fmt"
	"math/rand/v2"
	"testing"
)

// lockingGet is MemoryStore.Get with Lock in place of RLock, on the same
// store: the only difference between the two sub-benchmarks below.
func lockingGet(s *MemoryStore, id string) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	c := *j
	return &c, nil
}

// The Phase 2 question: does a read lock let readers overlap in a way an
// exclusive lock doesn't? Many goroutines (b.RunParallel, one per
// GOMAXPROCS) read the same 1,000 jobs from the same store, each starting
// at a random job and walking forward, with nothing shared between them
// but the store. Run with -cpu 1,8 to see one reader against eight.
//
// MemoryStore is no longer on GoFlow's production path (jobs live in
// PostgreSQL); this settles the old question, nothing more.
func BenchmarkMemoryStoreGet(b *testing.B) {
	s := NewMemoryStore()
	ids := make([]string, 1000)
	for i := range ids {
		j, err := New(fmt.Sprintf("job %d", i))
		if err != nil {
			b.Fatal(err)
		}
		if err := s.Add(j); err != nil {
			b.Fatal(err)
		}
		ids[i] = j.ID
	}
	for _, c := range []struct {
		name string
		get  func(string) (*Job, error)
	}{
		{"lock=RLock", s.Get},
		{"lock=Lock", func(id string) (*Job, error) { return lockingGet(s, id) }},
	} {
		b.Run(c.name, func(b *testing.B) {
			b.RunParallel(func(pb *testing.PB) {
				i := rand.IntN(len(ids))
				for pb.Next() {
					if _, err := c.get(ids[i]); err != nil {
						b.Error(err)
						return
					}
					i = (i + 1) % len(ids)
				}
			})
		})
	}
}
