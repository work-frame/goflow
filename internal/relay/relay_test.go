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

func (o *memOutbox) passCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.passes
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
	r := New(&memOutbox{}, &memQueue{}, discardLogger(), time.Millisecond)
	stop := r.Start(context.Background())

	finished := make(chan struct{})
	go func() { stop(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not return within 5s")
	}
}

// While everything works, the relay must not query the database between
// wake-ups: on a free database that sleeps when idle, a timer here would
// keep it awake around the clock and use up its monthly compute.
func TestRelayDoesNotPollWhileHealthy(t *testing.T) {
	o := &memOutbox{}
	stop := New(o, &memQueue{}, discardLogger(), time.Millisecond).Start(context.Background())
	defer stop()

	eventually(t, "the pass at start", func() bool { return o.passCount() >= 1 })
	time.Sleep(100 * time.Millisecond) // a polling relay would make dozens of passes here
	if got := o.passCount(); got != 1 {
		t.Fatalf("relay made %d passes with nothing to do, want 1 (the one at start)", got)
	}
}

func TestRetryWaitDoublesUpToTheCap(t *testing.T) {
	var got []time.Duration
	var d time.Duration
	for range 12 {
		d = nextRetry(d, time.Second)
		got = append(got, d)
	}
	want := []time.Duration{1, 2, 4, 8, 16, 32, 64, 128, 256, 300, 300, 300}
	for i := range want {
		if got[i] != want[i]*time.Second {
			t.Fatalf("waits = %v, want %v seconds", got, want)
		}
	}
}

// memStale hands out stale IDs like RequeueStale: oldest first, up to limit,
// and only marks them re-sent when send succeeds.
type memStale struct {
	mu    sync.Mutex
	stale []string
	runs  int
}

func (s *memStale) RequeueStale(ctx context.Context, _ time.Duration, limit int, send func(context.Context, []string) error) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs++
	batch := slices.Clone(s.stale[:min(limit, len(s.stale))])
	if len(batch) == 0 {
		return 0, nil
	}
	if err := send(ctx, batch); err != nil {
		return 0, err
	}
	s.stale = s.stale[len(batch):]
	return len(batch), nil
}

func (s *memStale) runCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs
}

func TestReconcilerResendsStaleJobsAtStart(t *testing.T) {
	s, q := &memStale{stale: []string{"lost1", "lost2"}}, &memQueue{}
	stop := NewReconciler(s, q, discardLogger(), time.Hour, 10*time.Minute).Start(context.Background())
	defer stop()

	eventually(t, "both lost IDs re-sent", func() bool { return slices.Equal(q.queued(), []string{"lost1", "lost2"}) })
}

func TestReconcilerRunsAgainOnItsInterval(t *testing.T) {
	s := &memStale{}
	stop := NewReconciler(s, &memQueue{}, discardLogger(), 5*time.Millisecond, time.Minute).Start(context.Background())
	defer stop()

	eventually(t, "three runs", func() bool { return s.runCount() >= 3 })
}

// memDelayed is a delayed set: due times by ID, promoted into queued.
type memDelayed struct {
	mu     sync.Mutex
	due    map[string]time.Time
	queued []string
}

func (d *memDelayed) PromoteDue(_ context.Context, now time.Time, limit int) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var ready []string
	for id, at := range d.due {
		if !at.After(now) {
			ready = append(ready, id)
		}
	}
	slices.Sort(ready)
	ready = ready[:min(limit, len(ready))]
	for _, id := range ready {
		delete(d.due, id)
		d.queued = append(d.queued, id)
	}
	return len(ready), nil
}

func (d *memDelayed) snapshot() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.queued)
}

func TestPromoterMovesRetriesWhenDue(t *testing.T) {
	d := &memDelayed{due: map[string]time.Time{
		"now":   time.Now().Add(-time.Second),
		"later": time.Now().Add(80 * time.Millisecond),
		"never": time.Now().Add(time.Hour),
	}}
	stop := NewPromoter(d, discardLogger(), 5*time.Millisecond).Start(context.Background())
	defer stop()

	eventually(t, "the due retry promoted", func() bool { return slices.Contains(d.snapshot(), "now") })
	if slices.Contains(d.snapshot(), "later") {
		t.Fatal("a retry was promoted before it was due")
	}
	eventually(t, "the second retry promoted once due", func() bool { return slices.Contains(d.snapshot(), "later") })
	if slices.Contains(d.snapshot(), "never") {
		t.Fatal("a retry due in an hour was promoted")
	}
}
