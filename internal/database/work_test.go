package database

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/work-frame/goflow/internal/job"
)

// testLease is long enough that no lease runs out during a test unless the
// test means it to.
const testLease = time.Minute

// claim claims id with a minute-long lease and reports whether it won.
func claim(t *testing.T, w *WorkStore, id string) bool {
	t.Helper()
	_, won, err := w.Claim(context.Background(), id, testLease)
	if err != nil {
		t.Fatalf("Claim(%s): %v", id, err)
	}
	return won
}

// leaseOf reads the job's current lease token ("" if it has none).
func leaseOf(t *testing.T, f *jobsFixture, id string) string {
	t.Helper()
	var token *string
	if err := f.db.QueryRowContext(context.Background(), `SELECT lease_token FROM jobs WHERE id = $1`, id).Scan(&token); err != nil {
		t.Fatalf("read lease of %s: %v", id, err)
	}
	if token == nil {
		return ""
	}
	return *token
}

func TestClaimThenComplete(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	w := NewWorkStore(f.db)
	id := addJobs(t, f, "do it")[0]

	if !claim(t, w, id) {
		t.Fatal("Claim of a pending job = false, want true")
	}
	if s := statusOf(t, f, id); s != job.StatusProcessing {
		t.Fatalf("status after Claim = %s, want processing", s)
	}
	if err := w.Complete(context.Background(), id, leaseOf(t, f, id)); err != nil {
		t.Fatal(err)
	}
	if s := statusOf(t, f, id); s != job.StatusCompleted {
		t.Fatalf("status after Complete = %s, want completed", s)
	}
	if token := leaseOf(t, f, id); token != "" {
		t.Errorf("a completed job still has a lease (%q)", token)
	}
}

// The second copy of a duplicate ID finds the job finished and skips it.
func TestClaimSkipsFinishedAndMissingJobs(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	w := NewWorkStore(f.db)
	ctx := context.Background()
	ids := addJobs(t, f, "done", "broken")

	complete := func(ctx context.Context, id, token string) error { return w.Complete(ctx, id, token) }
	bury := func(ctx context.Context, id, token string) error { return w.Bury(ctx, id, token, "broken for good") }
	for i, finish := range []func(context.Context, string, string) error{complete, bury} {
		claim(t, w, ids[i])
		if err := finish(ctx, ids[i], leaseOf(t, f, ids[i])); err != nil {
			t.Fatal(err)
		}
		if claim(t, w, ids[i]) {
			t.Errorf("Claim of a finished job (%s) = true, want false", statusOf(t, f, ids[i]))
		}
	}
	if claim(t, w, "cccccccccccccccccccccccccccccccc") {
		t.Error("Claim of a job that doesn't exist = true, want false")
	}
}

// A processing job is someone else's: claiming it again must lose.
func TestClaimRefusesAProcessingJob(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	w := NewWorkStore(f.db)
	id := addJobs(t, f, "taken")[0]

	if !claim(t, w, id) {
		t.Fatal("first Claim = false, want true")
	}
	if claim(t, w, id) {
		t.Fatal("second Claim of a processing job = true, want false: the job would run twice")
	}
}

// Two goroutines claim the same job at the same moment, over and over,
// against real PostgreSQL. Exactly one may win each time. The goroutines use
// separate pooled connections, so this is the same race as two processes.
func TestClaimHasExactlyOneWinner(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	w := NewWorkStore(f.db)
	ctx := context.Background()
	const rounds = 50

	ids := make([]string, rounds)
	for i := range rounds {
		ids[i] = addJobs(t, f, fmt.Sprintf("race %d", i))[0]
	}

	for i, id := range ids {
		start := make(chan struct{})
		results := make(chan bool, 2)
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start // both goroutines are ready; release them together
				_, won, err := w.Claim(ctx, id, testLease)
				if err != nil {
					errs <- err
					return
				}
				results <- won
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		close(errs)
		for err := range errs {
			t.Fatalf("round %d: Claim: %v", i, err)
		}
		winners := 0
		for won := range results {
			if won {
				winners++
			}
		}
		if winners != 1 {
			t.Fatalf("round %d: %d goroutines won the claim, want exactly 1", i, winners)
		}
	}
}

// jobRow reads the retry columns straight from the table, last_error included.
func jobRow(t *testing.T, f *jobsFixture, id string) (status string, attempts int, next *time.Time, lastError string) {
	t.Helper()
	var n, e = &time.Time{}, ""
	var nextNull, errNull bool
	row := f.db.QueryRowContext(context.Background(),
		`SELECT status, attempts, next_attempt_at IS NULL, COALESCE(next_attempt_at, now()), last_error IS NULL, COALESCE(last_error, '') FROM jobs WHERE id = $1`, id)
	if err := row.Scan(&status, &attempts, &nextNull, n, &errNull, &e); err != nil {
		t.Fatalf("read job %s: %v", id, err)
	}
	if !nextNull {
		next = n
	}
	return status, attempts, next, e
}

// Every claim counts an attempt, and a retrying job is claimable again.
func TestRetryThenClaimCountsAttempts(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	w := NewWorkStore(f.db)
	ctx := context.Background()
	id := addJobs(t, f, "flaky")[0]

	c, _, err := w.Claim(ctx, id, testLease)
	if err != nil || c.Attempt != 1 || c.MaxAttempts != job.DefaultMaxAttempts || c.Title != "flaky" {
		t.Fatalf("first Claim = %d of %d (%q), %v; want 1 of %d (flaky)", c.Attempt, c.MaxAttempts, c.Title, err, job.DefaultMaxAttempts)
	}
	before := time.Now()
	due, err := w.Retry(ctx, id, c.LeaseToken, 30*time.Second, "smtp: connection refused")
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if d := due.Sub(before); d < 25*time.Second || d > 40*time.Second {
		t.Errorf("retry due in %v, want about 30s", d)
	}
	status, attempts, next, lastError := jobRow(t, f, id)
	if status != "retrying" || attempts != 1 || next == nil || !next.Equal(due) || lastError != "smtp: connection refused" {
		t.Fatalf("after Retry: status %s, attempts %d, next %v, last_error %q", status, attempts, next, lastError)
	}

	c, _, err = w.Claim(ctx, id, testLease)
	if err != nil || c.Attempt != 2 {
		t.Fatalf("Claim of a retrying job = attempt %d, %v; want 2", c.Attempt, err)
	}
	if _, _, next, _ := jobRow(t, f, id); next != nil {
		t.Errorf("next_attempt_at = %v while processing, want NULL", next)
	}
}

func TestBuryMarksDeadAndKeepsTheError(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	w := NewWorkStore(f.db)
	ctx := context.Background()
	id := addJobs(t, f, "hopeless")[0]
	claim(t, w, id)

	long := strings.Repeat("x", 5000) // longer than last_error allows
	if err := w.Bury(ctx, id, leaseOf(t, f, id), long); err != nil {
		t.Fatalf("Bury: %v", err)
	}
	status, _, _, lastError := jobRow(t, f, id)
	if status != "dead" || len([]rune(lastError)) != maxErrorLength {
		t.Fatalf("after Bury: status %s, last_error length %d; want dead, %d", status, len([]rune(lastError)), maxErrorLength)
	}
	if claim(t, w, id) {
		t.Error("a dead job was claimed again")
	}
}

// Retry only applies to a job this worker is running.
func TestRetryNeedsAProcessingJob(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	w := NewWorkStore(f.db)
	id := addJobs(t, f, "not started")[0]

	if _, err := w.Retry(context.Background(), id, "no-such-lease", time.Second, "x"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Retry of a pending job = %v, want ErrLeaseLost", err)
	}
}

// The database refuses a retrying job without a due time.
func TestRetryingNeedsADueTime(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	id := addJobs(t, f, "no time")[0]
	_, err := f.db.ExecContext(context.Background(), `UPDATE jobs SET status = 'retrying' WHERE id = $1`, id)
	if err == nil || !strings.Contains(err.Error(), "jobs_retrying_has_due_time") {
		t.Fatalf("err = %v, want a jobs_retrying_has_due_time violation", err)
	}
}

// Redis lost an overdue retry: the reconciler sends it again; one that
// isn't overdue stays put.
func TestRequeueStaleResendsOverdueRetries(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	w := NewWorkStore(f.db)
	o := NewOutboxStore(f.db)
	ctx := context.Background()
	ids := addJobs(t, f, "lost retry", "fresh retry")
	for _, id := range ids {
		claim(t, w, id)
		if _, err := w.Retry(ctx, id, leaseOf(t, f, id), time.Second, "boom"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.ExecContext(ctx,
		`UPDATE jobs SET next_attempt_at = now() - interval '1 hour' WHERE id = $1`, ids[0]); err != nil {
		t.Fatal(err)
	}

	var sent []string
	n, err := o.RequeueStale(ctx, 10*time.Minute, 100, func(_ context.Context, b []string) error { sent = append(sent, b...); return nil })
	if err != nil || n != 1 || sent[0] != ids[0] {
		t.Fatalf("RequeueStale = %d %v, %v; want only %s", n, sent, err, ids[0])
	}
	if _, _, next, _ := jobRow(t, f, ids[0]); next == nil || time.Since(*next) > time.Minute {
		t.Errorf("next_attempt_at after re-send = %v, want about now", next)
	}
}
