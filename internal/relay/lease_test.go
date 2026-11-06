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

// leaseFakes plays both sides: Redis's deadlines and queue, and what
// PostgreSQL answers for each job.
type leaseFakes struct {
	mu        sync.Mutex
	deadlines map[string]time.Time // Redis: job ID → tracked deadline
	queued    []string             // Redis: IDs sent to the queue
	verdict   map[string]string    // PostgreSQL: "expired", "dead", "leased" or "gone"
	until     time.Time            // PostgreSQL's deadline for "leased" jobs
	asked     []string             // IDs PostgreSQL was asked about
	swept     int                  // ReclaimExpired calls
	sweepIDs  []string             // what the first sweep finds
	redisDown bool
}

func (f *leaseFakes) Enqueue(_ context.Context, ids ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queued = append(f.queued, ids...)
	return nil
}

func (f *leaseFakes) ExpiredLeases(_ context.Context, now time.Time, limit int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.redisDown {
		return nil, errors.New("dial tcp: connection refused")
	}
	var out []string
	for id, d := range f.deadlines {
		if !d.After(now) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (f *leaseFakes) TrackLease(_ context.Context, id string, until time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deadlines[id] = until
	return nil
}

func (f *leaseFakes) DropLease(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.deadlines, id)
	return nil
}

func (f *leaseFakes) Reclaim(_ context.Context, id string) (bool, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, id)
	switch f.verdict[id] {
	case "expired":
		f.verdict[id] = "gone" // taken back: not processing any more
		return true, time.Time{}, nil
	case "dead":
		f.verdict[id] = "gone"
		return false, time.Time{}, nil
	case "leased":
		return false, f.until, nil
	}
	return false, time.Time{}, nil
}

func (f *leaseFakes) ReclaimExpired(context.Context, int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.swept++
	ids := f.sweepIDs
	f.sweepIDs = nil
	return ids, nil
}

func (f *leaseFakes) snapshot() (deadlines map[string]time.Time, queued, asked []string, swept int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := map[string]time.Time{}
	for k, v := range f.deadlines {
		d[k] = v
	}
	return d, slices.Clone(f.queued), slices.Clone(f.asked), f.swept
}

func newLeaseFakes() *leaseFakes {
	return &leaseFakes{deadlines: map[string]time.Time{}, verdict: map[string]string{}}
}

func startReaper(t *testing.T, f *leaseFakes) {
	t.Helper()
	stop := NewLeaseReaper(f, f, slog.New(slog.DiscardHandler), 5*time.Millisecond, time.Hour).Start(context.Background())
	t.Cleanup(stop)
}

// Every deadline that passed is a question for PostgreSQL, and its answer
// decides: taken back (queue it), dead (just forget it), still leased
// (keep PostgreSQL's deadline), or no longer processing (forget it).
func TestReaperDoesWhatPostgreSQLSays(t *testing.T) {
	f := newLeaseFakes()
	past := time.Now().Add(-time.Second)
	for _, id := range []string{"crashed", "out-of-attempts", "renewed", "finished"} {
		f.deadlines[id] = past
	}
	f.verdict["crashed"] = "expired"
	f.verdict["out-of-attempts"] = "dead"
	f.verdict["renewed"] = "leased"
	f.until = time.Now().Add(time.Hour)
	f.deadlines["running"] = time.Now().Add(time.Hour) // not passed: not asked
	startReaper(t, f)

	eventually(t, "all four answered", func() bool { _, _, asked, _ := f.snapshot(); return len(asked) >= 4 })
	deadlines, queued, asked, _ := f.snapshot()
	if !slices.Equal(queued, []string{"crashed"}) {
		t.Errorf("queued %v, want only the job taken back", queued)
	}
	if slices.Contains(asked, "running") {
		t.Error("PostgreSQL was asked about a lease whose deadline hasn't passed")
	}
	if !deadlines["renewed"].Equal(f.until) {
		t.Errorf("renewed job's deadline %v, want PostgreSQL's %v", deadlines["renewed"], f.until)
	}
	for _, id := range []string{"crashed", "out-of-attempts", "finished"} {
		if _, ok := deadlines[id]; ok {
			t.Errorf("%s's deadline is still tracked", id)
		}
	}
}

// Redis's deadline passing is not expiry: if PostgreSQL says the lease is
// still running, the job is neither queued nor dropped, however many times
// the reaper asks.
func TestRedisNeverDecidesExpiry(t *testing.T) {
	f := newLeaseFakes()
	f.deadlines["job"] = time.Now().Add(-time.Hour) // Redis thinks it's long gone
	f.verdict["job"] = "leased"
	f.until = time.Now().Add(-time.Minute) // a skewed clock: still in the past here
	startReaper(t, f)

	eventually(t, "asked several times", func() bool { _, _, asked, _ := f.snapshot(); return len(asked) >= 5 })
	deadlines, queued, _, _ := f.snapshot()
	if len(queued) != 0 {
		t.Fatalf("queued %v although PostgreSQL said the lease is running", queued)
	}
	if _, ok := deadlines["job"]; !ok {
		t.Fatal("the deadline was dropped although the job is still leased")
	}
}

// With no deadline passed, PostgreSQL isn't asked anything: a sleeping
// free database stays asleep. Only the sweep at start touches it.
func TestQuietReaperLeavesPostgreSQLAlone(t *testing.T) {
	f := newLeaseFakes()
	f.deadlines["running"] = time.Now().Add(time.Hour)
	startReaper(t, f)

	time.Sleep(50 * time.Millisecond) // about ten checks
	_, _, asked, swept := f.snapshot()
	if len(asked) != 0 {
		t.Errorf("PostgreSQL asked about %v with no deadline passed", asked)
	}
	if swept != 1 {
		t.Errorf("%d sweeps, want 1 (at start)", swept)
	}
}

// The sweep at start finds expired leases Redis forgot (a Key Value
// restart) and sends them, even while Redis can't list deadlines.
func TestSweepFindsLeasesRedisLost(t *testing.T) {
	f := newLeaseFakes()
	f.sweepIDs = []string{"forgotten"}
	f.redisDown = true
	startReaper(t, f)

	eventually(t, "the forgotten job queued", func() bool { _, queued, _, _ := f.snapshot(); return slices.Equal(queued, []string{"forgotten"}) })
}
