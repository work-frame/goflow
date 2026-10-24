package job

import (
	"cmp"
	"errors"
	"slices"
	"sync"
)

var (
	ErrNotFound    = errors.New("job: not found")
	ErrDuplicateID = errors.New("job: duplicate id")
	ErrNilJob      = errors.New("job: nil job")
)

// MemoryStore keeps jobs in a map. It is safe for concurrent use.
//
// It stores and returns copies, so callers can never change stored jobs
// by modifying a *Job they hold.
type MemoryStore struct {
	mu   sync.RWMutex
	jobs map[string]*Job
}

// NewMemoryStore returns an empty, ready-to-use store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{jobs: make(map[string]*Job)}
}

// Add stores a copy of j. It fails if j is nil or its ID is already taken.
func (s *MemoryStore) Add(j *Job) error {
	if j == nil {
		return ErrNilJob
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.jobs[j.ID]; exists {
		return ErrDuplicateID
	}
	c := *j
	s.jobs[j.ID] = &c
	return nil
}

// Get returns a copy of the job with the given ID, or ErrNotFound.
func (s *MemoryStore) Get(id string) (*Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	j, ok := s.jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	c := *j
	return &c, nil
}

// List returns copies of all jobs, oldest first. It never returns nil.
func (s *MemoryStore) List() []*Job {
	s.mu.RLock()
	out := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		c := *j
		out = append(out, &c)
	}
	s.mu.RUnlock()

	// out is local to this call, so sorting needs no lock.
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
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.jobs[id]; !ok {
		return ErrNotFound
	}
	delete(s.jobs, id)
	return nil
}
