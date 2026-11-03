package database

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/work-frame/goflow/internal/job"
)

func addWithKey(t *testing.T, f *jobsFixture, owner, title, key string) (*job.Job, error) {
	t.Helper()
	j := mustNewJob(t, owner, title)
	j.IdempotencyKey = key
	return j, f.s.Add(context.Background(), j)
}

func countRows(t *testing.T, f *jobsFixture, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestAddWithAUsedKeyIsDuplicateKey(t *testing.T) {
	f := newJobsFixture(t)
	first, err := addWithKey(t, f, f.alice, "send invoice", "key-1")
	if err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if _, err := addWithKey(t, f, f.alice, "send invoice", "key-1"); !errors.Is(err, job.ErrDuplicateKey) {
		t.Fatalf("second Add = %v, want job.ErrDuplicateKey", err)
	}

	got, err := f.s.GetByIdempotencyKey(context.Background(), f.alice, "key-1")
	if err != nil || got.ID != first.ID {
		t.Fatalf("GetByIdempotencyKey = %v, %v; want job %s", got, err, first.ID)
	}
	if n := countRows(t, f, `SELECT count(*) FROM job_outbox`); n != 1 {
		t.Errorf("%d outbox rows, want 1: the refused Add must leave nothing behind", n)
	}
}

// A duplicate job ID is a different problem from a reused key, and the
// unique index name is what tells them apart.
func TestAddTellsDuplicateIDFromDuplicateKey(t *testing.T) {
	f := newJobsFixture(t)
	j, err := addWithKey(t, f, f.alice, "x", "key-a")
	if err != nil {
		t.Fatal(err)
	}
	again := *j
	again.IdempotencyKey = "key-b"
	if err := f.s.Add(context.Background(), &again); !errors.Is(err, job.ErrDuplicateID) {
		t.Fatalf("Add with a taken ID = %v, want job.ErrDuplicateID", err)
	}
}

func TestIdempotencyKeysBelongToOneUser(t *testing.T) {
	f := newJobsFixture(t)
	ctx := context.Background()
	a, err := addWithKey(t, f, f.alice, "alice's", "same")
	if err != nil {
		t.Fatal(err)
	}
	b, err := addWithKey(t, f, f.bob, "bob's", "same")
	if err != nil {
		t.Fatalf("bob can't use a key alice used: %v", err)
	}
	for owner, want := range map[string]string{f.alice: a.ID, f.bob: b.ID} {
		got, err := f.s.GetByIdempotencyKey(ctx, owner, "same")
		if err != nil || got.ID != want {
			t.Errorf("GetByIdempotencyKey(%s) = %v, %v; want %s", owner, got, err, want)
		}
	}
	if _, err := f.s.GetByIdempotencyKey(ctx, f.alice, "never-used"); !errors.Is(err, job.ErrNotFound) {
		t.Errorf("unknown key: err = %v, want job.ErrNotFound", err)
	}
}

// Jobs without a key store NULL, and NULLs never collide.
func TestJobsWithoutAKeyNeverCollide(t *testing.T) {
	f := newJobsFixture(t)
	addJobs(t, f, "one", "two", "three")
	if n := countRows(t, f, `SELECT count(*) FROM jobs WHERE idempotency_key IS NULL`); n != 3 {
		t.Errorf("%d jobs with a NULL key, want 3", n)
	}
}

// The real race: many requests with one key at the same moment, each on its
// own connection. PostgreSQL's unique index lets exactly one INSERT commit.
func TestConcurrentAddsWithOneKeyHaveOneWinner(t *testing.T) {
	f := newJobsFixture(t)
	const n = 20

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j := mustNewJob(t, f.alice, "charge card")
			j.IdempotencyKey = "pay-123"
			<-start // release them together
			errs[i] = f.s.Add(context.Background(), j)
		}()
	}
	close(start)
	wg.Wait()

	won, dup := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, job.ErrDuplicateKey):
			dup++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if won != 1 || dup != n-1 {
		t.Fatalf("%d won, %d duplicate; want 1 and %d", won, dup, n-1)
	}
	if got := countRows(t, f, `SELECT count(*) FROM jobs WHERE user_id = $1`, f.alice); got != 1 {
		t.Errorf("%d jobs stored, want 1", got)
	}
	if got := countRows(t, f, `SELECT count(*) FROM job_outbox`); got != 1 {
		t.Errorf("%d outbox rows, want 1: losers must roll back their outbox row too", got)
	}
}

func TestIdempotencyKeyLengthIsChecked(t *testing.T) {
	f := newJobsFixture(t)
	if _, err := addWithKey(t, f, f.alice, "x", strings.Repeat("k", 256)); err == nil {
		t.Fatal("a 256-character key was stored, want the CHECK to refuse it")
	}
}
