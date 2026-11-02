package relay

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"
)

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// memOutbox is an outbox in memory: Relay hands out the oldest IDs and
// removes them only when send succeeds, like the real one.
type memOutbox struct {
	mu     sync.Mutex
	ids    []string
	passes int
}

func (o *memOutbox) add(ids ...string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ids = append(o.ids, ids...)
}

func (o *memOutbox) waiting() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.ids)
}

func (o *memOutbox) Relay(ctx context.Context, limit int, send func(context.Context, []string) error) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.passes++
	batch := slices.Clone(o.ids[:min(limit, len(o.ids))])
	if len(batch) == 0 {
		return 0, nil
	}
	if err := send(ctx, batch); err != nil {
		return 0, err
	}
	o.ids = o.ids[len(batch):]
	return len(batch), nil
}

// memQueue records IDs; while down is true, every Enqueue fails.
type memQueue struct {
	mu   sync.Mutex
	ids  []string
	down bool
}

func (q *memQueue) Enqueue(_ context.Context, ids ...string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.down {
		return errors.New("redis: connection refused")
	}
	q.ids = append(q.ids, ids...)
	return nil
}

func (q *memQueue) setDown(down bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.down = down
}

func (q *memQueue) queued() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.ids)
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

func TestRelaySendsWhatIsWaitingAtStart(t *testing.T) {
	o, q := &memOutbox{}, &memQueue{}
	o.add("a", "b")
	stop := New(o, q, discardLogger(), time.Hour).Start(context.Background())
	defer stop()

	eventually(t, "a and b queued", func() bool { return slices.Equal(q.queued(), []string{"a", "b"}) })
}

// With an hour between ticks, only Wake can explain a quick delivery.
func TestWakeSendsNewJobsWithoutWaitingForATick(t *testing.T) {
	o, q := &memOutbox{}, &memQueue{}
	r := New(o, q, discardLogger(), time.Hour)
	stop := r.Start(context.Background())
	defer stop()

	o.add("new")
	r.Wake()
	eventually(t, "new queued after Wake", func() bool { return slices.Equal(q.queued(), []string{"new"}) })
}

func TestWakeNeverBlocks(t *testing.T) {
	r := New(&memOutbox{}, &memQueue{}, discardLogger(), time.Hour) // not started: nobody reads wake

	done := make(chan struct{})
	go func() {
		for range 1000 {
			r.Wake()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wake blocked with nobody listening: the API would hang creating jobs")
	}
}

// While Redis is down nothing is lost: the IDs wait, and a later tick sends them.
func TestRelayRetriesAfterTheQueueComesBack(t *testing.T) {
	o, q := &memOutbox{}, &memQueue{down: true}
	o.add("waits")
	stop := New(o, q, discardLogger(), 5*time.Millisecond).Start(context.Background())
	defer stop()

	eventually(t, "a few failed passes", func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		return o.passes >= 3
	})
	if o.waiting() != 1 {
		t.Fatalf("outbox lost the ID while Redis was down")
	}

	q.setDown(false)
	eventually(t, "waits queued once Redis is back", func() bool { return slices.Equal(q.queued(), []string{"waits"}) })
}

func TestRelayDrainsMoreThanOneBatch(t *testing.T) {
	o, q := &memOutbox{}, &memQueue{}
	for range BatchSize*2 + 5 {
		o.add("x")
	}
	stop := New(o, q, discardLogger(), time.Hour).Start(context.Background())
	defer stop()

	eventually(t, "every batch sent in one pass", func() bool { return len(q.queued()) == BatchSize*2+5 })
}

func TestStopEndsTheLoop(t *testing.T) {
	o := &memOutbox{}
	stop := New(o, &memQueue{}, discardLogger(), time.Millisecond).Start(context.Background())
	eventually(t, "first pass", func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		return o.passes >= 1
	})

	stop() // returns only after the goroutine has exited
	o.mu.Lock()
	after := o.passes
	o.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.passes != after {
		t.Errorf("relay ran %d more passes after stop", o.passes-after)
	}
}
