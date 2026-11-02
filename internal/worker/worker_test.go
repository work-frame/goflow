package worker

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/work-frame/goflow/internal/queue"
)

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// memQueue behaves like the Redis lists: Take moves an ID from pending to
// processing, Ack removes it, Recover moves processing back to the front.
type memQueue struct {
	mu         sync.Mutex
	pending    []string // index 0 is taken first
	processing []string
}

func (q *memQueue) Take(ctx context.Context, timeout time.Duration) (string, error) {
	q.mu.Lock()
	if len(q.pending) > 0 {
		id := q.pending[0]
		q.pending = q.pending[1:]
		q.processing = append(q.processing, id)
		q.mu.Unlock()
		return id, nil
	}
	q.mu.Unlock()
	select { // nothing waiting: block briefly, like BLMOVE
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(5 * time.Millisecond):
		return "", queue.ErrEmpty
	}
}

func (q *memQueue) Ack(_ context.Context, id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if i := slices.Index(q.processing, id); i >= 0 {
		q.processing = slices.Delete(q.processing, i, i+1)
	}
	return nil
}

func (q *memQueue) Recover(context.Context) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.processing)
	q.pending = append(slices.Clone(q.processing), q.pending...)
	q.processing = nil
	return n, nil
}

func (q *memQueue) lists() (pending, processing []string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.pending), slices.Clone(q.processing)
}

// memStore tracks job statuses like WorkStore does.
type memStore struct {
	mu     sync.Mutex
	status map[string]string
}

func newMemStore(ids ...string) *memStore {
	s := &memStore{status: map[string]string{}}
	for _, id := range ids {
		s.status[id] = "queued"
	}
	return s
}

func (s *memStore) Claim(_ context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.status[id] {
	case "pending", "queued", "processing":
		s.status[id] = "processing"
		return true, nil
	}
	return false, nil
}

func (s *memStore) set(id, from, to string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status[id] == from {
		s.status[id] = to
	}
}

func (s *memStore) Complete(_ context.Context, id string) error {
	s.set(id, "processing", "completed")
	return nil
}

func (s *memStore) Fail(_ context.Context, id string) error {
	s.set(id, "processing", "failed")
	return nil
}

func (s *memStore) get(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status[id]
}

// runUntil runs w until done reports true (or 5 s pass), then stops it.
func runUntil(t *testing.T, w *Worker, what string, done func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- w.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			cancel()
			<-finished
			t.Fatalf("after 5s: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-finished; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// counter counts handler calls per job ID.
type counter struct {
	mu    sync.Mutex
	calls map[string]int
}

func (c *counter) handle(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[id]++
	return nil
}

func (c *counter) count(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[id]
}

func TestWorkerCompletesAndAcks(t *testing.T) {
	q := &memQueue{pending: []string{"job1"}}
	s := newMemStore("job1")
	c := &counter{}
	w := &Worker{Queue: q, Store: s, Handle: c.handle, Logger: discardLogger()}

	runUntil(t, w, "job1 completed", func() bool { return s.get("job1") == "completed" })

	if c.count("job1") != 1 {
		t.Errorf("handler ran %d times, want 1", c.count("job1"))
	}
	if _, processing := q.lists(); len(processing) != 0 {
		t.Errorf("processing list = %v after completion, want empty", processing)
	}
}

// The same ID twice (at-least-once delivery) runs the job once.
func TestWorkerSkipsADuplicateID(t *testing.T) {
	q := &memQueue{pending: []string{"dup", "dup", "last"}}
	s := newMemStore("dup", "last")
	c := &counter{}
	w := &Worker{Queue: q, Store: s, Handle: c.handle, Logger: discardLogger()}

	runUntil(t, w, "every ID handled", func() bool {
		pending, processing := q.lists()
		return s.get("last") == "completed" && len(pending) == 0 && len(processing) == 0
	})
	if got := c.count("dup"); got != 1 {
		t.Fatalf("duplicate job ran %d times, want 1", got)
	}
}

func TestWorkerMarksAFailingJobFailed(t *testing.T) {
	q := &memQueue{pending: []string{"bad"}}
	s := newMemStore("bad")
	w := &Worker{Queue: q, Store: s, Logger: discardLogger(),
		Handle: func(context.Context, string) error { return errors.New("boom") }}

	runUntil(t, w, "bad marked failed", func() bool { return s.get("bad") == "failed" })
	if _, processing := q.lists(); len(processing) != 0 {
		t.Errorf("processing list = %v, want empty: the failure was recorded", processing)
	}
}

// A worker stopped mid-job leaves the ID in processing. The next worker puts
// it back and runs the job to the end.
func TestWorkerRecoversAJobLeftMidWay(t *testing.T) {
	q := &memQueue{pending: []string{"long"}}
	s := newMemStore("long")

	started := make(chan struct{})
	first := &Worker{Queue: q, Store: s, Logger: discardLogger(),
		Handle: func(ctx context.Context, _ string) error {
			close(started)
			<-ctx.Done() // the "crash": stopped before finishing
			return ctx.Err()
		}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- first.Run(ctx) }()
	<-started
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if _, processing := q.lists(); !slices.Equal(processing, []string{"long"}) {
		t.Fatalf("processing list after the crash = %v, want [long]", processing)
	}

	c := &counter{}
	second := &Worker{Queue: q, Store: s, Handle: c.handle, Logger: discardLogger()}
	runUntil(t, second, "long completed after restart", func() bool { return s.get("long") == "completed" })
}
