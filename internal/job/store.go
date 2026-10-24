package job

import (
	"cmp"
	"errors"
	"slices"
)

var (
	ErrNotFound    = errors.New("job: not found")
	ErrDuplicateID = errors.New("job: duplicate id")
	ErrNilJob      = errors.New("job: nil job")
)

// MemoryStore keeps jobs in a map.
// It is NOT safe for concurrent use. Locking arrives in Phase 2,
// when HTTP handlers start calling it from many goroutines.
type MemoryStore struct {
	jobs map[string]*Job
}

// NewMemoryStore returns an empty, ready-to-use store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{jobs: make(map[string]*Job)}
}

// Add stores j. It fails if j is nil or its ID is already taken.
func (s *MemoryStore) Add(j *Job) error {
	if j == nil {
		return ErrNilJob
	}
	if _, exists := s.jobs[j.ID]; exists {
		return ErrDuplicateID
	}
	s.jobs[j.ID] = j
	return nil
}

// Get returns the job with the given ID, or ErrNotFound.
func (s *MemoryStore) Get(id string) (*Job, error) {
	j, ok := s.jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	return j, nil
}

// List returns all jobs, oldest first. It never returns nil.
func (s *MemoryStore) List() []*Job {
	out := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, j)
	}
	slices.SortFunc(out, func(a, b *Job) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return out
}

// Delete removes the job with the given ID, or returns ErrNotFound.
func (s *MemoryStore) Delete(id string) error {
	if _, ok := s.jobs[id]; !ok {
		return ErrNotFound
	}
	delete(s.jobs, id)
	return nil
}
