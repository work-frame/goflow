package database

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/work-frame/goflow/internal/job"
)

// expiredClaim claims id with a lease that has already run out, so the
// next Reclaim may take it back.
func expiredClaim(t *testing.T, w *WorkStore, id string) Claimed {
	t.Helper()
	c, won, err := w.Claim(context.Background(), id, -time.Second)
	if err != nil || !won {
		t.Fatalf("Claim(%s) = %v, %v; want a win", id, won, err)
	}
	return c
}

func TestEveryClaimGetsAFreshToken(t *testing.T) {
	f := newJobsFixture(t)
	w := NewWorkStore(f.db)
	ctx := context.Background()
	id := addJobs(t, f, "twice")[0]

	first := expiredClaim(t, w, id)
	if _, _, err := w.Reclaim(ctx, id); err != nil {
		t.Fatal(err)
	}
	second, _, err := w.Claim(ctx, id, testLease)
	if err != nil {
		t.Fatal(err)
	}
	if first.LeaseToken == "" || second.LeaseToken == "" || first.LeaseToken == second.LeaseToken {
		t.Fatalf("tokens %q and %q: each claim needs its own", first.LeaseToken, second.LeaseToken)
	}
	if got := leaseOf(t, f, id); got != second.LeaseToken {
		t.Errorf("stored token %q, want the second claim's", got)
	}
}

// The scenario leases exist for. A holds the job, its lease runs out, the
// job is taken back and B claims it. A wakes up: everything it tries is
// refused and changes nothing, while B's lease is untouched; then B
// finishes normally.
func TestAStaleHolderCanChangeNothing(t *testing.T) {
	f := newJobsFixture(t)
	w := NewWorkStore(f.db)
	ctx := context.Background()
	id := addJobs(t, f, "contested")[0]

	a := expiredClaim(t, w, id)
	if requeue, _, err := w.Reclaim(ctx, id); err != nil || !requeue {
		t.Fatalf("Reclaim = %v, %v; want the job taken back", requeue, err)
	}
	b, won, err := w.Claim(ctx, id, testLease)
	if err != nil || !won {
		t.Fatalf("B's Claim = %v, %v", won, err)
	}
	before := versionOf(t, f, id)

	stale := map[string]error{}
	_, stale["Renew"] = w.Renew(ctx, id, a.LeaseToken, testLease)
	stale["Complete"] = w.Complete(ctx, id, a.LeaseToken)
	_, stale["Retry"] = w.Retry(ctx, id, a.LeaseToken, time.Second, "stale")
	stale["Bury"] = w.Bury(ctx, id, a.LeaseToken, "stale")
	for op, err := range stale {
		if !errors.Is(err, ErrLeaseLost) {
			t.Errorf("A's %s = %v, want ErrLeaseLost", op, err)
		}
	}
	if s := statusOf(t, f, id); s != job.StatusProcessing {
		t.Fatalf("after A's attempts: status %s, want processing (B's)", s)
	}
	if got := leaseOf(t, f, id); got != b.LeaseToken {
		t.Fatalf("after A's attempts: lease %q, want B's %q", got, b.LeaseToken)
	}
	if v := versionOf(t, f, id); v != before {
		t.Fatalf("A's refused writes moved the version from %d to %d", before, v)
	}

	if err := w.Complete(ctx, id, b.LeaseToken); err != nil {
		t.Fatalf("B's Complete: %v", err)
	}
	if s := statusOf(t, f, id); s != job.StatusCompleted {
		t.Errorf("status %s, want completed", s)
	}
}

// Only PostgreSQL's clock counts: a lease it says is still running isn't
// taken back, whatever the caller thinks, and the caller learns when it
// runs out.
func TestReclaimLeavesALiveLeaseAlone(t *testing.T) {
	f := newJobsFixture(t)
	w := NewWorkStore(f.db)
	ctx := context.Background()
	id := addJobs(t, f, "running")[0]
	c, _, err := w.Claim(ctx, id, testLease)
	if err != nil {
		t.Fatal(err)
	}

	requeue, until, err := w.Reclaim(ctx, id)
	if err != nil || requeue {
		t.Fatalf("Reclaim of a live lease = %v, %v; want it left alone", requeue, err)
	}
	if !until.Equal(c.LeaseExpires) {
		t.Errorf("Reclaim says leased until %v, Claim said %v", until, c.LeaseExpires)
	}
	if d := time.Until(until); d < 50*time.Second || d > 70*time.Second {
		t.Errorf("lease runs out in %v, want about a minute", d)
	}
	if got := leaseOf(t, f, id); got != c.LeaseToken || statusOf(t, f, id) != job.StatusProcessing {
		t.Error("Reclaim changed a live lease")
	}
	if requeue, until, err := w.Reclaim(ctx, "cccccccccccccccccccccccccccccccc"); err != nil || requeue || !until.IsZero() {
		t.Errorf("Reclaim of a job that doesn't exist = %v, %v, %v; want nothing", requeue, until, err)
	}
}

// Renewing moves the deadline forward, and only for the holder.
func TestRenewExtendsTheLease(t *testing.T) {
	f := newJobsFixture(t)
	w := NewWorkStore(f.db)
	ctx := context.Background()
	id := addJobs(t, f, "long job")[0]
	c := expiredClaim(t, w, id)

	until, err := w.Renew(ctx, id, c.LeaseToken, testLease)
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if d := time.Until(until); d < 50*time.Second {
		t.Errorf("renewed until %v from now, want about a minute", d)
	}
	// The renewal saved it: the lease isn't expired any more.
	if requeue, _, err := w.Reclaim(ctx, id); err != nil || requeue {
		t.Errorf("Reclaim after a renewal = %v, %v; want it left alone", requeue, err)
	}
}

// Until somebody takes it back, an expired lease still belongs to its
// holder: no other worker can have the job, so its result is accepted.
func TestAnExpiredLeaseCountsUntilTakenBack(t *testing.T) {
	f := newJobsFixture(t)
	w := NewWorkStore(f.db)
	id := addJobs(t, f, "late")[0]
	c := expiredClaim(t, w, id)
	if err := w.Complete(context.Background(), id, c.LeaseToken); err != nil {
		t.Fatalf("Complete on an expired, untaken lease = %v, want accepted", err)
	}
}

// A lost lease uses up the attempt, like a crash. On the last attempt the
// job is dead, with a reason, instead of going round forever.
func TestALostLastAttemptBuriesTheJob(t *testing.T) {
	f := newJobsFixture(t)
	w := NewWorkStore(f.db)
	ctx := context.Background()
	id := addJobs(t, f, "keeps crashing")[0]
	if _, err := f.db.ExecContext(ctx, `UPDATE jobs SET max_attempts = 1 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	expiredClaim(t, w, id)

	requeue, until, err := w.Reclaim(ctx, id)
	if err != nil || requeue || !until.IsZero() {
		t.Fatalf("Reclaim = %v, %v, %v; want the job taken back but not queued", requeue, until, err)
	}
	status, attempts, _, lastError := jobRow(t, f, id)
	if status != "dead" || attempts != 1 || lastError != LostLeaseError {
		t.Errorf("after the last lease ran out: %s, %d attempts, %q; want dead, 1, %q", status, attempts, lastError, LostLeaseError)
	}
	if leaseOf(t, f, id) != "" {
		t.Error("a dead job still has a lease")
	}
}

// The race at the heart of leases, against real PostgreSQL: the holder of
// an expired lease records its result at the moment a reaper takes the job
// back. Exactly one wins, every time. Never both (the job completed AND
// queued to run again), never neither.
func TestCompleteAndReclaimRaceHasOneWinner(t *testing.T) {
	f := newJobsFixture(t)
	w := NewWorkStore(f.db)
	ctx := context.Background()

	for round := range 30 {
		id := addJobs(t, f, "race")[0]
		c := expiredClaim(t, w, id)

		var completeErr, reclaimErr error
		var requeued bool
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; completeErr = w.Complete(ctx, id, c.LeaseToken) }()
		go func() { defer wg.Done(); <-start; requeued, _, reclaimErr = w.Reclaim(ctx, id) }()
		close(start)
		wg.Wait()

		if reclaimErr != nil || (completeErr != nil && !errors.Is(completeErr, ErrLeaseLost)) {
			t.Fatalf("round %d: Complete %v, Reclaim %v", round, completeErr, reclaimErr)
		}
		completed := completeErr == nil
		status := statusOf(t, f, id)
		switch {
		case completed && !requeued && status == job.StatusCompleted:
		case !completed && requeued && status == job.StatusQueued:
		default:
			t.Fatalf("round %d: Complete won %v, Reclaim won %v, status %s: want exactly one winner", round, completed, requeued, status)
		}
	}
}

// The same race between a heartbeat and a reaper: either the lease is
// renewed and stays, or it's taken back and the holder hears it's gone.
func TestRenewAndReclaimRaceHasOneWinner(t *testing.T) {
	f := newJobsFixture(t)
	w := NewWorkStore(f.db)
	ctx := context.Background()

	for round := range 30 {
		id := addJobs(t, f, "race")[0]
		c := expiredClaim(t, w, id)

		var renewErr, reclaimErr error
		var requeued bool
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _, renewErr = w.Renew(ctx, id, c.LeaseToken, testLease) }()
		go func() { defer wg.Done(); <-start; requeued, _, reclaimErr = w.Reclaim(ctx, id) }()
		close(start)
		wg.Wait()

		if reclaimErr != nil || (renewErr != nil && !errors.Is(renewErr, ErrLeaseLost)) {
			t.Fatalf("round %d: Renew %v, Reclaim %v", round, renewErr, reclaimErr)
		}
		renewed := renewErr == nil
		token := leaseOf(t, f, id)
		switch {
		case renewed && !requeued && token == c.LeaseToken:
		case !renewed && requeued && token == "":
		default:
			t.Fatalf("round %d: renewed %v, taken back %v, lease %q: want exactly one winner", round, renewed, requeued, token)
		}
	}
}

// Two reapers taking back the same jobs: each job goes back once.
func TestReclaimExpiredSendsEachJobOnce(t *testing.T) {
	f := newJobsFixture(t)
	w := NewWorkStore(f.db)
	ctx := context.Background()
	ids := addJobs(t, f, "a", "b", "c", "d", "live")
	for _, id := range ids[:4] {
		expiredClaim(t, w, id)
	}
	claim(t, w, ids[4])

	var mu sync.Mutex
	sent := map[string]int{}
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := w.ReclaimExpired(ctx, 100)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, id := range got {
				sent[id]++
			}
		}()
	}
	wg.Wait()
	for _, id := range ids[:4] {
		if sent[id] != 1 {
			t.Errorf("job %s sent %d times, want once", id, sent[id])
		}
	}
	if sent[ids[4]] != 0 || statusOf(t, f, ids[4]) != job.StatusProcessing {
		t.Error("a live lease was taken back")
	}
}

func TestProcessingReportsLeasedJobs(t *testing.T) {
	f := newJobsFixture(t)
	w := NewWorkStore(f.db)
	ids := addJobs(t, f, "running", "waiting")
	claim(t, w, ids[0])
	got, err := w.Processing(context.Background(), append(ids, "cccccccccccccccccccccccccccccccc"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[ids[0]] {
		t.Errorf("Processing = %v, want only %s", got, ids[0])
	}
}

// The database itself refuses a processing job without an owner, and a
// lease left on a job that isn't processing.
func TestDatabaseTiesLeasesToProcessing(t *testing.T) {
	f := newJobsFixture(t)
	ctx := context.Background()
	id := addJobs(t, f, "rules")[0]
	for name, stmt := range map[string]string{
		"processing without a lease": `UPDATE jobs SET status = 'processing' WHERE id = $1`,
		"a lease on a pending job":   `UPDATE jobs SET lease_token = 'x', lease_expires_at = now() WHERE id = $1`,
		"a token without an expiry":  `UPDATE jobs SET status = 'processing', lease_token = 'x' WHERE id = $1`,
	} {
		_, err := f.db.ExecContext(ctx, stmt, id)
		if err == nil || !strings.Contains(err.Error(), "jobs_processing_has_lease") {
			t.Errorf("%s: err = %v, want a jobs_processing_has_lease violation", name, err)
		}
	}
}

// Jobs left 'processing' before leases existed get one that has already
// run out, so the first sweep takes them back.
func TestMigration10GivesOldProcessingJobsAnExpiredLease(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	upTo9 := fstest.MapFS{}
	matches, err := fs.Glob(migrationFiles, "migrations/000[1-9]_*.sql")
	if err != nil || len(matches) != 9 {
		t.Fatalf("migrations 1-9: %v, %v", matches, err)
	}
	for _, name := range matches {
		body, err := fs.ReadFile(migrationFiles, name)
		if err != nil {
			t.Fatal(err)
		}
		upTo9[name] = &fstest.MapFile{Data: body}
	}
	if _, err := migrate(ctx, db, upTo9); err != nil {
		t.Fatalf("migrate to version 9: %v", err)
	}
	u := mustNewUser(t, "old@example.com")
	if err := NewUserStore(db).Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	for id, status := range map[string]string{
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": "processing",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb": "completed",
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO jobs (id, user_id, title, status, attempts, created_at, updated_at) VALUES ($1, $2, 'old', $3, 1, now(), now())`, id, u.ID, status); err != nil {
			t.Fatalf("insert %s job: %v", status, err)
		}
	}
	if _, err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	requeue, err := NewWorkStore(db).ReclaimExpired(ctx, 10)
	if err != nil || len(requeue) != 1 || requeue[0] != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("first sweep = %v, %v; want the old processing job", requeue, err)
	}
}
