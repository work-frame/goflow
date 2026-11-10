package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/work-frame/goflow/internal/job"
)

// versionOf reads a job's version straight from the table.
func versionOf(t *testing.T, f *jobsFixture, id string) int64 {
	t.Helper()
	var v int64
	if err := f.db.QueryRowContext(context.Background(), `SELECT version FROM jobs WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatalf("read version of %s: %v", id, err)
	}
	return v
}

// Every change a job goes through adds exactly 1 to its version, and the
// announced job carries the version the database has.
func TestEveryChangeRaisesTheVersion(t *testing.T) {
	t.Parallel()
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
	want := int64(1)
	if j.Version != want {
		t.Fatalf("new job version %d, want 1", j.Version)
	}

	var token string
	claimAs := func(leaseFor time.Duration) error {
		c, won, err := work.Claim(ctx, id, leaseFor)
		if err == nil && !won {
			err = fmt.Errorf("claim lost")
		}
		token = c.LeaseToken
		return err
	}
	steps := []struct {
		name string
		do   func() error
	}{
		{"Relay", func() error { _, err := outbox.Relay(ctx, 10, noop); return err }},
		{"RequeueStale", func() error {
			// A negative age makes every queued job count as stale right now.
			_, err := outbox.RequeueStale(ctx, -time.Hour, 10, noop)
			return err
		}},
		{"Claim", func() error { return claimAs(testLease) }},
		{"Renew", func() error { _, err := work.Renew(ctx, id, token, testLease); return err }},
		{"Retry", func() error { _, err := work.Retry(ctx, id, token, time.Second, "boom"); return err }},
		{"second Claim", func() error { return claimAs(testLease) }},
		{"Bury", func() error { return work.Bury(ctx, id, token, "boom") }},
		{"Rerun", func() error { _, err := jobs.Rerun(ctx, f.alice, id); return err }},
		{"third Claim, a lease already run out", func() error { return claimAs(-time.Second) }},
		{"Reclaim", func() error { _, _, err := work.Reclaim(ctx, id); return err }},
		{"fourth Claim", func() error { return claimAs(testLease) }},
		{"Complete", func() error { return work.Complete(ctx, id, token) }},
	}
	for _, step := range steps {
		r.take()
		if err := step.do(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		want++
		if got := versionOf(t, f, id); got != want {
			t.Fatalf("after %s: version %d, want %d", step.name, got, want)
		}
		r.mu.Lock()
		// RequeueStale and Renew change no status, so they announce nothing.
		if n := len(r.jobs); n > 0 && step.name != "RequeueStale" && step.name != "Renew" {
			if heard := r.jobs[n-1].Version; heard != want {
				t.Errorf("after %s: announced version %d, database has %d", step.name, heard, want)
			}
		}
		r.mu.Unlock()
	}

	// A change that changes nothing doesn't count.
	work.Complete(ctx, id, token) // already completed: 0 rows, ErrLeaseLost
	if got := versionOf(t, f, id); got != want {
		t.Errorf("a no-op Complete moved the version to %d", got)
	}
}

// The behaviour test above covers today's statements. This one covers
// tomorrow's: any UPDATE of jobs written in this package must raise the
// version, or a client would keep an older copy of the job over a newer one.
func TestEveryJobUpdateBumpsTheVersion(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	update := regexp.MustCompile("(?s)UPDATE jobs SET[^`]*")
	found := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, stmt := range update.FindAllString(string(src), -1) {
			found++
			if !strings.Contains(stmt, "version = version + 1") {
				t.Errorf("%s: an UPDATE of jobs doesn't raise the version:\n%s", name, stmt)
			}
		}
	}
	if found < 8 {
		t.Errorf("found %d UPDATE statements on jobs, want at least 8: is the pattern still right?", found)
	}
}

func TestReadsReturnTheVersion(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	ctx := context.Background()
	id := addJobs(t, f, "read me")[0]
	claim(t, NewWorkStore(f.db), id)

	got, err := f.s.Get(ctx, f.alice, id)
	if err != nil || got.Version != 2 {
		t.Fatalf("Get = version %v, %v; want 2", got, err)
	}
	page, err := f.s.List(ctx, f.alice, job.ListParams{Limit: 10})
	if err != nil || len(page.Jobs) != 1 || page.Jobs[0].Version != 2 {
		t.Fatalf("List = %v, %v; want one job at version 2", page.Jobs, err)
	}
}
