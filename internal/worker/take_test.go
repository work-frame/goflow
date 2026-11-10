package worker

import (
	"context"
	"sync"
	"testing"
	"time"
)

// timeoutQueue is a memQueue that records the timeout of every Take.
type timeoutQueue struct {
	*memQueue
	mu       sync.Mutex
	timeouts []time.Duration
}

func (q *timeoutQueue) Take(ctx context.Context, timeout time.Duration) (string, error) {
	q.mu.Lock()
	q.timeouts = append(q.timeouts, timeout)
	q.mu.Unlock()
	return q.memQueue.Take(ctx, timeout)
}

func (q *timeoutQueue) seen() []time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]time.Duration(nil), q.timeouts...)
}

// Every Take waits as long as the pool's TakeTimeout says, and 2 s when it
// isn't set: cmd/worker and the API in production never set it, so their
// idle workers keep sending one BLMOVE per 2 s, not one per second (Task 52).
func TestTakeWaitsThePoolsTakeTimeout(t *testing.T) {
	// A literal, not DefaultTakeTimeout: changing the constant must fail here.
	if DefaultTakeTimeout != 2*time.Second {
		t.Fatalf("DefaultTakeTimeout = %v, want 2s", DefaultTakeTimeout)
	}
	for _, c := range []struct {
		name      string
		set, want time.Duration
	}{
		{"unset", 0, 2 * time.Second},
		{"set", time.Second, time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			q := &timeoutQueue{memQueue: &memQueue{}}
			p := &Pool{Queue: q, Store: newMemStore(), Handle: (&counter{}).handle, Logger: discardLogger(),
				Workers: 2, DrainTimeout: time.Second, TakeTimeout: c.set}
			stop := start(t, p)
			eventually(t, "a few takes", func() bool { return len(q.seen()) >= 4 })
			stop()
			for _, got := range q.seen() {
				if got != c.want {
					t.Fatalf("Take waited %v, want %v (TakeTimeout %v)", got, c.want, c.set)
				}
			}
		})
	}
}
