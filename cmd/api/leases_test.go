package main

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/work-frame/goflow/internal/database"
	"github.com/work-frame/goflow/internal/queue"
	"github.com/work-frame/goflow/internal/relay"
	"github.com/work-frame/goflow/internal/worker"
)

// Three API instances, each running two workers, share one PostgreSQL and
// one Redis. The second and third start while the first is in the middle
// of its jobs. Before leases, each start reset every 'processing' job, so
// the first instance's jobs ran again (attempt 2) while still running.
// Now every job runs exactly once.
func TestSeveralProcessesRunEachJobOnce(t *testing.T) {
	dbURL := testDatabaseURL(t)
	redisURL := testRedisURL(t)
	newConfig := func() config {
		cfg := testConfig("127.0.0.1:0", dbURL)
		cfg.RedisURL = redisURL
		cfg.Workers = 2
		return cfg
	}
	first, _ := startServerWith(t, newConfig())
	client := testClient(t, 5*time.Second)
	token := signUp(t, client, first, dbURL)

	var ids []string
	for range 6 {
		ids = append(ids, createJob(t, client, first, token, "sleep:600ms"))
	}
	deadline := time.Now().Add(10 * time.Second)
	for running := 0; running == 0; {
		for _, id := range ids {
			if s, _ := jobState(t, client, first, token, id); s == "processing" {
				running++
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no job started within 10s")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Two more processes start while the first one's jobs are running.
	startServerWith(t, newConfig())
	startServerWith(t, newConfig())

	deadline = time.Now().Add(30 * time.Second)
	for _, id := range ids {
		for {
			s, attempts := jobState(t, client, first, token, id)
			if s == "completed" {
				if attempts != 1 {
					t.Errorf("job %s completed on attempt %d, want 1: another process's start ran it again", id, attempts)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("job %s still %s after 30s", id, s)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// spyStore is the real WorkStore, recording claims and what every
// Complete returned.
type spyStore struct {
	*database.WorkStore
	mu        sync.Mutex
	claims    []database.Claimed
	completes []error
}

func (s *spyStore) Claim(ctx context.Context, id string, leaseFor time.Duration) (database.Claimed, bool, error) {
	c, won, err := s.WorkStore.Claim(ctx, id, leaseFor)
	if won {
		s.mu.Lock()
		s.claims = append(s.claims, c)
		s.mu.Unlock()
	}
	return c, won, err
}

func (s *spyStore) Complete(ctx context.Context, id, token string) error {
	err := s.WorkStore.Complete(ctx, id, token)
	s.mu.Lock()
	s.completes = append(s.completes, err)
	s.mu.Unlock()
	return err
}

func (s *spyStore) state() (claims []database.Claimed, completes []error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]database.Claimed(nil), s.claims...), append([]error(nil), s.completes...)
}

// leaseRow reads a job's status, lease token and version straight from
// PostgreSQL.
func leaseRow(t *testing.T, ctx context.Context, db *sql.DB, id string) (status, token string, version int64) {
	t.Helper()
	var tok *string
	if err := db.QueryRowContext(ctx, `SELECT status, lease_token, version FROM jobs WHERE id = $1`, id).Scan(&status, &tok, &version); err != nil {
		t.Fatal(err)
	}
	if tok != nil {
		token = *tok
	}
	return status, token, version
}

// The race leases exist for, end to end on real PostgreSQL and Redis, with
// two worker pools as two processes would run them. Pool A's worker
// freezes (its heartbeat never runs, like a process stopped by a long
// pause), so its lease runs out. The reaper takes the job back; pool B
// claims it and is still running it when A wakes up and tries to record
// its result. A's write is refused and changes nothing; B's job stays B's,
// and B's result is the one recorded.
func TestAFrozenWorkerCannotOverwriteItsReplacement(t *testing.T) {
	dbURL := testDatabaseURL(t)
	redisURL := testRedisURL(t)
	cfg := testConfig("127.0.0.1:0", dbURL)
	cfg.RedisURL = redisURL // relay only: this server runs no workers
	base, _ := startServerWith(t, cfg)
	client := testClient(t, 5*time.Second)
	token := signUp(t, client, base, dbURL)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	db, err := database.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	q, err := queue.Open(ctx, redisURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	work := database.NewWorkStore(db)
	stopReaper := relay.NewLeaseReaper(work, q, discardLogger(), 20*time.Millisecond, time.Hour).Start(ctx)
	t.Cleanup(stopReaper)

	startPool := func(store worker.Store, lease, beat time.Duration, h worker.Handler) {
		poolCtx, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			p := &worker.Pool{Queue: q, Store: store, Handle: h, Logger: discardLogger(), Workers: 1,
				DrainTimeout: time.Second, Lease: lease, HeartbeatEvery: beat, TakeTimeout: testTakeTimeout}
			if err := p.Run(poolCtx); err != nil {
				t.Errorf("pool: %v", err)
			}
		}()
		t.Cleanup(func() { stop(); <-done })
	}

	id := createJob(t, client, base, token, "contested")
	aStarted, aMayFinish := make(chan struct{}), make(chan struct{})
	a := &spyStore{WorkStore: work}
	// A 300ms lease, and a heartbeat that won't come for a minute: frozen.
	startPool(a, 300*time.Millisecond, time.Minute, func(context.Context, worker.Run) error {
		close(aStarted)
		<-aMayFinish
		return nil
	})
	select {
	case <-aStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("pool A didn't start the job within 10s")
	}

	// The lease runs out on PostgreSQL's clock; the reaper sends the job
	// again. B claims it and holds it.
	bStarted, bMayFinish := make(chan struct{}), make(chan struct{})
	b := &spyStore{WorkStore: work}
	startPool(b, 30*time.Second, 0, func(context.Context, worker.Run) error {
		close(bStarted)
		<-bMayFinish
		return nil
	})
	select {
	case <-bStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("pool B didn't get the job within 10s of A's lease running out")
	}
	bClaims, _ := b.state()
	if len(bClaims) != 1 || bClaims[0].Attempt != 2 {
		t.Fatalf("B's claims %+v, want one, attempt 2", bClaims)
	}
	// Not one moment early: B claimed after A's lease ran out, both times
	// by PostgreSQL's clock. A never renewed; B's deadline is its claim
	// time plus its 30-second lease.
	aClaims, _ := a.state()
	if len(aClaims) != 1 {
		t.Fatalf("A's claims %+v, want one", aClaims)
	}
	aExpired := aClaims[0].LeaseExpires
	bClaimed := bClaims[0].LeaseExpires.Add(-30 * time.Second)
	if !bClaimed.After(aExpired) {
		t.Fatalf("B claimed the job at %s, %s before A's lease ran out at %s: a still-valid lease was taken back",
			bClaimed.Format(time.RFC3339Nano), aExpired.Sub(bClaimed), aExpired.Format(time.RFC3339Nano))
	}
	_, _, before := leaseRow(t, ctx, db, id)

	// A wakes up and tries to record its result while B is running.
	close(aMayFinish)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, completes := a.state(); len(completes) == 1 {
			if !errors.Is(completes[0], database.ErrLeaseLost) {
				t.Fatalf("A's late Complete = %v, want ErrLeaseLost: a worker whose lease ran out finished the job B is running", completes[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("A never tried to record its result")
		}
		time.Sleep(5 * time.Millisecond)
	}
	status, lease, version := leaseRow(t, ctx, db, id)
	if status != "processing" || lease != bClaims[0].LeaseToken || version != before {
		t.Fatalf("after A's late write: %s under %q, version %d (was %d); want B's job untouched", status, lease, version, before)
	}

	close(bMayFinish)
	deadline = time.Now().Add(5 * time.Second)
	for {
		s, attempts := jobState(t, client, base, token, id)
		if s == "completed" {
			if attempts != 2 {
				t.Errorf("completed with %d attempts, want 2 (A's lost one, B's)", attempts)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s after B finished, want completed", s)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
