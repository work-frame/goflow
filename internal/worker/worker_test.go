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
	case <-time.After(2 * time.Millisecond):
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

func (q *memQueue) add(ids ...string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending = append(q.pending, ids...)
}

func (q *memQueue) lists() (pending, processing []string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.pending), slices.Clone(q.processing)
}

// memStore tracks statuses like WorkStore: Claim wins only from pending or
// queued, under a mutex, so exactly one caller wins.
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
	case "pending", "queued":
		s.status[id] = "processing"
		return true, nil
	}
	return false, nil
}

func (s *memStore) ResetProcessing(context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for id, st := range s.status {
		if st == "processing" {
			s.status[id] = "queued"
			n++
		}
	}
	return n, nil
}

func (s *memStore) setIf(id, from, to string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status[id] == from {
		s.status[id] = to
	}
}

func (s *memStore) Complete(_ context.Context, id string) error {
	s.setIf(id, "processing", "completed")
	return nil
}

func (s *memStore) Fail(_ context.Context, id string) error {
	s.setIf(id, "processing", "failed")
	return nil
}

func (s *memStore) get(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status[id]
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

func newPool(q *memQueue, s *memStore, workers int, h Handler) *Pool {
	return &Pool{Queue: q, Store: s, Handle: h, Logger: discardLogger(), Workers: workers, DrainTimeout: time.Second}
}

// start runs p in the background and returns a function that stops it
// (cancels its context, the SIGTERM equivalent) and waits for Run to return.
func start(t *testing.T, p *Pool) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Run: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("Run did not return within 10s of stopping")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("after 5s: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPoolCompletesAndAcks(t *testing.T) {
	q, s, c := &memQueue{pending: []string{"job1"}}, newMemStore("job1"), &counter{}
	start(t, newPool(q, s, 2, c.handle))

	eventually(t, "job1 completed and acknowledged", func() bool {
		_, processing := q.lists()
		return s.get("job1") == "completed" && len(processing) == 0
	})
	if c.count("job1") != 1 {
		t.Errorf("handler ran %d times, want 1", c.count("job1"))
	}
}

// With 4 workers, 4 jobs run at the same time: each handler waits until all
// four have started. With fewer workers this would never finish.
func TestPoolRunsJobsConcurrently(t *testing.T) {
	ids := []string{"a", "b", "c", "d"}
	q, s := &memQueue{pending: slices.Clone(ids)}, newMemStore(ids...)

	var started sync.WaitGroup
	started.Add(len(ids))
	allStarted := make(chan struct{})
	go func() { started.Wait(); close(allStarted) }()

	start(t, newPool(q, s, len(ids), func(ctx context.Context, _ string) error {
		started.Done()
		select {
		case <-allStarted:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}))

	eventually(t, "all four jobs completed together", func() bool {
		for _, id := range ids {
			if s.get(id) != "completed" {
				return false
			}
		}
		return true
	})
}

// The same ID twice (at-least-once delivery), taken by different workers:
// the job runs once.
func TestPoolRunsADuplicateIDOnce(t *testing.T) {
	q, s, c := &memQueue{pending: []string{"dup", "dup", "last"}}, newMemStore("dup", "last"), &counter{}
	start(t, newPool(q, s, 3, c.handle))

	eventually(t, "every ID handled", func() bool {
		pending, processing := q.lists()
		return s.get("last") == "completed" && len(pending) == 0 && len(processing) == 0
	})
	if got := c.count("dup"); got != 1 {
		t.Fatalf("duplicate job ran %d times, want 1", got)
	}
}

func TestPoolMarksAFailingJobFailed(t *testing.T) {
	q, s := &memQueue{pending: []string{"bad"}}, newMemStore("bad")
	start(t, newPool(q, s, 1, func(context.Context, string) error { return errors.New("boom") }))

	eventually(t, "bad marked failed and acknowledged", func() bool {
		_, processing := q.lists()
		return s.get("bad") == "failed" && len(processing) == 0
	})
}

// SIGTERM stops new claims but lets an active job finish. The active job's
// context must stay alive, and a job queued after the stop is never taken.
func TestStopLetsTheActiveJobFinish(t *testing.T) {
	q, s := &memQueue{pending: []string{"active"}}, newMemStore("active", "later")

	inJob := make(chan struct{})
	release := make(chan struct{})
	var ctxErrAtEnd error
	p := newPool(q, s, 1, func(ctx context.Context, _ string) error {
		close(inJob)
		<-release
		ctxErrAtEnd = ctx.Err()
		return nil
	})
	p.DrainTimeout = 5 * time.Second
	stop := start(t, p)

	<-inJob
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	time.Sleep(20 * time.Millisecond) // the stop signal is out; the job is still running
	q.add("later")
	close(release)
	<-stopped

	if ctxErrAtEnd != nil {
		t.Fatalf("the active job's context was cancelled (%v): SIGTERM must not cancel running jobs", ctxErrAtEnd)
	}
	if got := s.get("active"); got != "completed" {
		t.Errorf("active job status = %s, want completed", got)
	}
	if got := s.get("later"); got != "queued" {
		t.Errorf("a job queued after the stop has status %s, want queued: it must not be taken", got)
	}
}

// A job still running when the drain timeout ends is cancelled, records
// nothing, and keeps its ID in the processing list for the next start.
func TestDrainTimeoutCancelsAndKeepsTheJob(t *testing.T) {
	q, s := &memQueue{pending: []string{"slow"}}, newMemStore("slow")
	inJob := make(chan struct{})
	p := newPool(q, s, 1, func(ctx context.Context, _ string) error {
		close(inJob)
		<-ctx.Done() // only returns once cancelled
		return ctx.Err()
	})
	p.DrainTimeout = 50 * time.Millisecond
	stop := start(t, p)

	<-inJob
	began := time.Now()
	stop()
	if took := time.Since(began); took < 50*time.Millisecond {
		t.Errorf("Run returned after %v, before the 50ms drain timeout", took)
	}
	if got := s.get("slow"); got != "processing" {
		t.Errorf("status after the drain timeout = %s, want processing (nothing recorded)", got)
	}
	if _, processing := q.lists(); !slices.Equal(processing, []string{"slow"}) {
		t.Errorf("processing list = %v, want [slow]: the ID must survive for the next start", processing)
	}
}

// Startup recovery: a job a stopped process left 'processing' (in PostgreSQL
// and in Redis's processing list) is reset, re-queued and run to the end.
func TestStartupRecoveryRunsLeftoverJobs(t *testing.T) {
	q := &memQueue{processing: []string{"leftover"}}
	s := newMemStore()
	s.status["leftover"] = "processing"
	c := &counter{}
	start(t, newPool(q, s, 2, c.handle))

	eventually(t, "leftover completed after recovery", func() bool { return s.get("leftover") == "completed" })
	if c.count("leftover") != 1 {
		t.Errorf("leftover ran %d times, want 1", c.count("leftover"))
	}
}

func TestPoolNeedsAWorker(t *testing.T) {
	err := newPool(&memQueue{}, newMemStore(), 0, (&counter{}).handle).Run(context.Background())
	if err == nil {
		t.Fatal("Run with 0 workers succeeded, want an error")
	}
}

// flakyQueue fails Recover until up is set, like a Redis that isn't
// reachable yet when the process starts.
type flakyQueue struct {
	*memQueue
	mu       sync.Mutex
	up       bool
	attempts int
}

func (q *flakyQueue) Recover(ctx context.Context) (int, error) {
	q.mu.Lock()
	q.attempts++
	up := q.up
	q.mu.Unlock()
	if !up {
		return 0, errors.New("dial tcp: lookup red-abc123: no such host")
	}
	return q.memQueue.Recover(ctx)
}

func (q *flakyQueue) state() (attempts int, up bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.attempts, q.up
}

// A pool that starts while Redis is unreachable keeps retrying its startup
// recovery, runs nothing meanwhile, and starts working once Redis is back.
func TestPoolWaitsForRedisAtStartup(t *testing.T) {
	q := &flakyQueue{memQueue: &memQueue{pending: []string{"waiting"}}}
	s, c := newMemStore("waiting"), &counter{}
	p := newPool(nil, s, 2, c.handle)
	p.Queue = q
	p.RetryWait = time.Millisecond
	start(t, p)

	eventually(t, "a few failed recoveries", func() bool { n, _ := q.state(); return n >= 3 })
	if c.count("waiting") != 0 {
		t.Fatal("a job ran before startup recovery succeeded")
	}

	q.mu.Lock()
	q.up = true
	q.mu.Unlock()
	eventually(t, "the waiting job completed once Redis was up", func() bool { return s.get("waiting") == "completed" })
}

// Stopping while still waiting for Redis returns promptly and cleanly.
func TestPoolStopsWhileWaitingForRedis(t *testing.T) {
	q := &flakyQueue{memQueue: &memQueue{}}
	p := newPool(nil, newMemStore(), 1, (&counter{}).handle)
	p.Queue = q
	p.RetryWait = time.Hour
	stop := start(t, p)

	eventually(t, "the first failed recovery", func() bool { n, _ := q.state(); return n >= 1 })
	began := time.Now()
	stop()
	if took := time.Since(began); took > time.Second {
		t.Errorf("stop took %v while waiting for Redis, want well under 1s", took)
	}
}
