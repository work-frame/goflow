package job

import (
	"cmp"
	"errors"
	"slices"
	"sync"
	"time"
)

var (
	ErrNotFound    = errors.New("job: not found")
	ErrDuplicateID = errors.New("job: duplicate id")
	ErrNilJob      = errors.New("job: nil job")
	// ErrDuplicateKey: the owner already has a job with this idempotency key.
	ErrDuplicateKey = errors.New("job: duplicate idempotency key")
	// ErrNotRerunnable: only a dead (or old failed) job can be re-run.
	ErrNotRerunnable = errors.New("job: only a dead or failed job can be re-run")
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
	if j.IdempotencyKey != "" && s.findByKey(j.UserID, j.IdempotencyKey) != nil {
		return ErrDuplicateKey
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

// GetByIdempotencyKey returns a copy of userID's job created with key, or
// ErrNotFound.
func (s *MemoryStore) GetByIdempotencyKey(userID, key string) (*Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	j := s.findByKey(userID, key)
	if j == nil {
		return nil, ErrNotFound
	}
	c := *j
	return &c, nil
}

// findByKey scans every job. Fine for tests; PostgreSQL uses an index.
// The caller holds s.mu.
func (s *MemoryStore) findByKey(userID, key string) *Job {
	for _, j := range s.jobs {
		if j.UserID == userID && j.IdempotencyKey == key {
			return j
		}
	}
	return nil
}

// Rerun puts a dead or failed job back to pending with its attempts reset,
// and returns a copy. Same rules as database.JobStore.Rerun.
func (s *MemoryStore) Rerun(id string) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	j, ok := s.jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	if !j.Status.Rerunnable() {
		return nil, ErrNotRerunnable
	}
	j.Status = StatusPending
	j.Attempts = 0
	j.NextAttemptAt = nil
	j.UpdatedAt = time.Now().UTC().Truncate(time.Microsecond)
	c := *j
	return &c, nil
}
