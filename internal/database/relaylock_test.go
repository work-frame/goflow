package database

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Phase 17: the relay and the reconciler must hold no row lock while they
// talk to Redis. Each test stands in for Redis with a send that blocks until
// the test lets it go, the way a Redis that accepts a connection and never
// answers would (FAILURES.md, case 66).

// heldSend is a send function that reports when it is called, then waits
// until release (or until its context ends).
type heldSend struct {
	entered chan []string
	release chan struct{}
	once    sync.Once
}

func newHeldSend(t *testing.T) *heldSend {
	t.Helper()
	h := &heldSend{entered: make(chan []string, 1), release: make(chan struct{})}
	t.Cleanup(h.let) // never leave a goroutine blocked behind a failed test
	return h
}

func (h *heldSend) send(ctx context.Context, ids []string) error {
	h.entered <- ids
	select {
	case <-h.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// let releases the send; calling it again does nothing.
func (h *heldSend) let() { h.once.Do(func() { close(h.release) }) }

// waitEntered fails the test unless send is called within 5 seconds.
func (h *heldSend) waitEntered(t *testing.T, what string) []string {
	t.Helper()
	select {
	case ids := <-h.entered:
		return ids
	case <-time.After(5 * time.Second):
		t.Fatalf("after 5s: %s", what)
		return nil
	}
}

// expireOutboxClaims makes every outbox claim look expired, as if the relay
// that took them had stopped answering for longer than a claim lasts.
func expireOutboxClaims(t *testing.T, f *jobsFixture) {
	t.Helper()
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE job_outbox SET claimed_until = now() - interval '1 second' WHERE claimed_until IS NOT NULL`); err != nil {
		t.Errorf("expire the outbox claims: %v (Task 72 adds claim columns to job_outbox)", err)
	}
}

// requestDeadline is what a request gets for the database (internal/api).
const requestDeadline = 3 * time.Second

// While the relay waits for Redis, deleting the job must not wait for it.
func TestDeleteDoesNotWaitForARelaySend(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	withConns(f.db, 6)
	o := NewOutboxStore(f.db)
	id := addJobs(t, f, "deleted while its ID is on the way")[0]

	h := newHeldSend(t)
	relayed := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := o.Relay(ctx, 100, h.send)
		relayed <- err
	}()
	h.waitEntered(t, "the relay never called send")

	ctx, cancel := context.WithTimeout(context.Background(), requestDeadline)
	defer cancel()
	start := time.Now()
	err := f.s.Delete(ctx, f.alice, id)
	took := time.Since(start)
	h.let()
	relayErr := <-relayed

	if err != nil || took > time.Second {
		t.Errorf("Delete while the relay waited for Redis took %v, err %v; want under 1s: the relay holds the job's outbox row locked while it talks to Redis", took.Round(time.Millisecond), err)
	}
	if relayErr != nil {
		t.Errorf("the relay, after its job was deleted: %v; want no error, with nothing left to finish", relayErr)
	}
	if n := waiting(t, o); n != 0 {
		t.Errorf("outbox rows left: %d, want 0", n)
	}
}

// Window 1: Redis hangs before the ID is published. The claim runs out,
// another relay sends the ID, and the first relay, when its send finally
// returns, must not finish (or undo) the second one's claim.
func TestAnExpiredRelayClaimIsSentAgainAndNotStolenBack(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	withConns(f.db, 6)
	o := NewOutboxStore(f.db)
	id := addJobs(t, f, "first relay hangs")[0]

	first, second := newHeldSend(t), newHeldSend(t)
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	var firstN, secondN int
	go func() {
		var err error
		firstN, err = o.Relay(context.Background(), 100, first.send)
		firstDone <- err
	}()
	first.waitEntered(t, "the first relay never called send")
	expireOutboxClaims(t, f)

	go func() {
		var err error
		secondN, err = o.Relay(context.Background(), 100, second.send)
		secondDone <- err
	}()
	if ids := second.waitEntered(t, "a second relay never re-sent the job whose claim ran out (the first relay still holds it)"); len(ids) != 1 || ids[0] != id {
		t.Fatalf("second relay sent %v, want [%s]", ids, id)
	}

	first.let() // Redis finally answers the first relay
	if err := <-firstDone; err != nil || firstN != 0 {
		t.Errorf("first relay after losing its claim = %d, %v; want 0, nil: it must finish nothing", firstN, err)
	}
	if n := waiting(t, o); n != 1 {
		t.Errorf("after the first relay returned: %d outbox rows, want 1: an expired claim deleted another relay's row", n)
	}
	if s := statusOf(t, f, id); s != "pending" {
		t.Errorf("status after the first relay returned = %s, want pending: an expired claim marked the job", s)
	}

	second.let()
	if err := <-secondDone; err != nil || secondN != 1 {
		t.Errorf("second relay = %d, %v; want 1, nil", secondN, err)
	}
	if n := waiting(t, o); n != 0 {
		t.Errorf("after the second relay: %d outbox rows, want 0", n)
	}
	if s := statusOf(t, f, id); s != "queued" {
		t.Errorf("status after the second relay = %s, want queued", s)
	}
}

// Window 2: Redis accepts the ID, then the relay stops before it records
// that. Another relay sends the ID again, and Redis holds it twice; the
// job still runs once, because Claim lets one worker win.
func TestAnIDSentTwiceRunsOnce(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	withConns(f.db, 6)
	o := NewOutboxStore(f.db)
	w := NewWorkStore(f.db)
	id := addJobs(t, f, "published, then the relay stopped")[0]

	var redis []string // what Redis would hold
	ctx, stop := context.WithCancel(context.Background())
	_, err := o.Relay(ctx, 100, func(_ context.Context, ids []string) error {
		redis = append(redis, ids...)
		stop() // the relay stops right after Redis accepted the IDs
		return nil
	})
	if err == nil {
		t.Fatal("Relay stopped mid-pass reported success")
	}
	if n := waiting(t, o); n != 1 {
		t.Fatalf("outbox rows after the stopped pass: %d, want 1: the row must wait to be sent again", n)
	}
	expireOutboxClaims(t, f)

	n, err := o.Relay(context.Background(), 100, func(_ context.Context, ids []string) error {
		redis = append(redis, ids...)
		return nil
	})
	if err != nil || n != 1 {
		t.Fatalf("second Relay = %d, %v; want 1, nil", n, err)
	}
	if len(redis) != 2 || redis[0] != id || redis[1] != id {
		t.Fatalf("Redis holds %v, want the ID twice", redis)
	}

	// Two workers take one copy each.
	wins := 0
	for range redis {
		if claim(t, w, id) {
			wins++
		}
	}
	if wins != 1 {
		t.Errorf("claims won for one job sent twice: %d, want 1", wins)
	}
}

// While the reconciler waits for Redis, a worker must be able to claim the
// job it is re-sending, and the reconciler must not then overwrite the
// claimed job.
func TestAWorkerClaimsWhileTheReconcilerSends(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	withConns(f.db, 6)
	o := NewOutboxStore(f.db)
	w := NewWorkStore(f.db)
	bg := context.Background()
	id := addJobs(t, f, "queued an hour ago")[0]
	relayAll(t, o)
	if _, err := f.db.ExecContext(bg,
		`UPDATE jobs SET created_at = now() - interval '2 hours', updated_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}

	h := newHeldSend(t)
	reconciled := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(bg, 10*time.Second)
		defer cancel()
		_, err := o.RequeueStale(ctx, 10*time.Minute, 100, h.send)
		reconciled <- err
	}()
	h.waitEntered(t, "the reconciler never called send")

	ctx, cancel := context.WithTimeout(bg, requestDeadline)
	defer cancel()
	start := time.Now()
	c, won, err := w.Claim(ctx, id, testLease)
	took := time.Since(start)
	if err != nil || !won || took > time.Second {
		t.Errorf("Claim while the reconciler waited for Redis took %v: won %v, err %v; want won in under 1s: the reconciler holds the job's row locked while it talks to Redis", took.Round(time.Millisecond), won, err)
	}

	var before time.Time
	var versionBefore int64
	if err := f.db.QueryRowContext(bg, `SELECT updated_at, version FROM jobs WHERE id = $1`, id).Scan(&before, &versionBefore); err != nil {
		t.Fatal(err)
	}
	h.let()
	if err := <-reconciled; err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("reconciler after the job was claimed: %v", err)
	}

	var status, token string
	var after time.Time
	var versionAfter int64
	if err := f.db.QueryRowContext(bg, `SELECT status, coalesce(lease_token, ''), updated_at, version FROM jobs WHERE id = $1`, id).Scan(&status, &token, &after, &versionAfter); err != nil {
		t.Fatal(err)
	}
	if won && (status != "processing" || token != c.LeaseToken) {
		t.Errorf("after the reconciler: status %s, lease %q; want processing under the worker's lease %q", status, token, c.LeaseToken)
	}
	if !after.Equal(before) || versionAfter != versionBefore {
		t.Errorf("the reconciler overwrote a newer state: updated_at %v → %v, version %d → %d; want both unchanged", before, after, versionBefore, versionAfter)
	}
}
