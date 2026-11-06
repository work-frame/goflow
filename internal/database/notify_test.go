package database

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/work-frame/goflow/internal/job"
)

// recorder is a Notifier that writes down what it hears, as
// "status:id" for changes and "deleted:id" for deletions.
type recorder struct {
	mu     sync.Mutex
	events []string
	jobs   []*job.Job
}

func (r *recorder) JobChanged(_ context.Context, j *job.Job) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, string(j.Status)+":"+j.ID)
	r.jobs = append(r.jobs, j)
}

func (r *recorder) JobDeleted(_ context.Context, _, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, "deleted:"+id)
}

// take returns what was heard since the last call, and forgets it.
func (r *recorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	return out
}

func expectEvents(t *testing.T, r *recorder, step string, want ...string) {
	t.Helper()
	got := r.take()
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("after %s: heard %v, want %v", step, got, want)
	}
}

// Every status change a job goes through is heard once, after it is
// committed, with the job as the database has it.
func TestStoresAnnounceEveryStatusChange(t *testing.T) {
	f := newJobsFixture(t)
	ctx := context.Background()
	r := &recorder{}
	jobs := f.s.WithNotifier(r)
	outbox := NewOutboxStore(f.db).WithNotifier(r)
	work := NewWorkStore(f.db).WithNotifier(r)
	noop := func(context.Context, []string) error { return nil }

	j := mustNewJob(t, f.alice, "flaky")
	if err := jobs.Add(ctx, j); err != nil {
		t.Fatal(err)
	}
	id := j.ID
	expectEvents(t, r, "Add", "pending:"+id)

	if _, err := outbox.Relay(ctx, 10, noop); err != nil {
		t.Fatal(err)
	}
	expectEvents(t, r, "Relay", "queued:"+id)

	claim(t, work, id)
	expectEvents(t, r, "Claim", "processing:"+id)
	if _, err := work.Retry(ctx, id, leaseOf(t, f, id), time.Second, "boom"); err != nil {
		t.Fatal(err)
	}
	expectEvents(t, r, "Retry", "retrying:"+id)
	claim(t, work, id)
	expectEvents(t, r, "second Claim", "processing:"+id)
	if err := work.Bury(ctx, id, leaseOf(t, f, id), "boom"); err != nil {
		t.Fatal(err)
	}
	expectEvents(t, r, "Bury", "dead:"+id)

	if _, err := jobs.Rerun(ctx, f.alice, id); err != nil {
		t.Fatal(err)
	}
	expectEvents(t, r, "Rerun", "pending:"+id)
	if _, err := outbox.Relay(ctx, 10, noop); err != nil {
		t.Fatal(err)
	}
	claim(t, work, id)
	if err := work.Complete(ctx, id, leaseOf(t, f, id)); err != nil {
		t.Fatal(err)
	}
	expectEvents(t, r, "Relay, Claim, Complete", "queued:"+id, "processing:"+id, "completed:"+id)

	if err := jobs.Delete(ctx, f.alice, id); err != nil {
		t.Fatal(err)
	}
	expectEvents(t, r, "Delete", "deleted:"+id)
}

// The announced job is the committed row: owner, attempts and times included.
func TestAnnouncedJobMatchesTheDatabase(t *testing.T) {
	f := newJobsFixture(t)
	ctx := context.Background()
	r := &recorder{}
	work := NewWorkStore(f.db).WithNotifier(r)
	id := addJobs(t, f, "check me")[0]

	claim(t, work, id)
	stored, err := f.s.Get(ctx, f.alice, id)
	if err != nil {
		t.Fatal(err)
	}
	heard := r.jobs[len(r.jobs)-1]
	if heard.UserID != f.alice || heard.Attempts != 1 || heard.Title != "check me" || !heard.UpdatedAt.Equal(stored.UpdatedAt) {
		t.Errorf("heard %+v, database has %+v", heard, stored)
	}
}

// Nothing is announced when nothing changed.
func TestNoAnnouncementWithoutAChange(t *testing.T) {
	f := newJobsFixture(t)
	ctx := context.Background()
	r := &recorder{}
	jobs := f.s.WithNotifier(r)
	work := NewWorkStore(f.db).WithNotifier(r)
	id := addJobs(t, f, "done")[0]
	claim(t, work, id)
	token := leaseOf(t, f, id)
	if err := work.Complete(ctx, id, token); err != nil {
		t.Fatal(err)
	}
	r.take()

	claim(t, work, id) // already completed: 0 rows
	// The lease ended with the job: every write with it is refused.
	if err := work.Complete(ctx, id, token); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("second Complete = %v, want ErrLeaseLost", err)
	}
	if err := work.Bury(ctx, id, token, "x"); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("Bury of a completed job = %v, want ErrLeaseLost", err)
	}
	if requeue, _, err := work.Reclaim(ctx, id); err != nil || requeue {
		t.Errorf("Reclaim of a completed job = %v, %v; want nothing", requeue, err)
	}
	jobs.Rerun(ctx, f.alice, id) // not dead: refused
	jobs.Rerun(ctx, f.bob, id)   // not bob's
	jobs.Delete(ctx, f.bob, id)  // not bob's
	dup := mustNewJob(t, f.alice, "dup")
	dup.ID = id
	jobs.Add(ctx, dup) // duplicate ID: rolled back
	expectEvents(t, r, "changes that changed nothing")
}

// Jobs taken back from an expired lease are announced as queued again;
// renewing a lease announces nothing (the status didn't change).
func TestReclaimAnnouncesEachJob(t *testing.T) {
	f := newJobsFixture(t)
	ctx := context.Background()
	r := &recorder{}
	work := NewWorkStore(f.db).WithNotifier(r)
	ids := addJobs(t, f, "a", "b", "untouched")
	for _, id := range ids[:2] {
		if _, _, err := work.Claim(ctx, id, -time.Second); err != nil { // already run out
			t.Fatal(err)
		}
	}
	claim(t, work, ids[2])
	r.take()

	if _, err := work.Renew(ctx, ids[2], leaseOf(t, f, ids[2]), testLease); err != nil {
		t.Fatal(err)
	}
	expectEvents(t, r, "Renew")
	requeue, err := work.ReclaimExpired(ctx, 10)
	if err != nil || len(requeue) != 2 {
		t.Fatalf("ReclaimExpired = %v, %v; want the two expired jobs", requeue, err)
	}
	got := r.take()
	if len(got) != 2 || !strings.HasPrefix(got[0], "queued:") || !strings.HasPrefix(got[1], "queued:") {
		t.Errorf("heard %v, want two queued events", got)
	}
}

// Stores built without a notifier work as before.
func TestStoresWithoutANotifier(t *testing.T) {
	f := newJobsFixture(t)
	id := addJobs(t, f, "quiet")[0]
	w := NewWorkStore(f.db)
	claim(t, w, id)
	if err := w.Complete(context.Background(), id, leaseOf(t, f, id)); err != nil {
		t.Fatal(err)
	}
}

// NotifyAll tells every notifier, in order, and skips nil ones.
func TestNotifyAllFansOut(t *testing.T) {
	a, b := &recorder{}, &recorder{}
	n := NotifyAll(a, nil, b)
	n.JobChanged(context.Background(), &job.Job{ID: "j1", Status: job.StatusQueued})
	n.JobDeleted(context.Background(), "u", "j1")
	for name, r := range map[string]*recorder{"a": a, "b": b} {
		if got := strings.Join(r.take(), " "); got != "queued:j1 deleted:j1" {
			t.Errorf("%s heard %q", name, got)
		}
	}
}
