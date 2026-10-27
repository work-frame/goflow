package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/work-frame/goflow/internal/job"
)

// jobsFixture is a JobStore on a private copy of the migrated template, with
// two registered users who each own their own jobs.
type jobsFixture struct {
	db    *sql.DB
	s     *JobStore
	alice string
	bob   string
}

func newJobsFixture(t *testing.T) *jobsFixture {
	t.Helper()
	db := newMigratedTestDB(t)
	users := NewUserStore(db)
	f := &jobsFixture{db: db, s: NewJobStore(db)}
	for email, id := range map[string]*string{"alice@example.com": &f.alice, "bob@example.com": &f.bob} {
		u := mustNewUser(t, email)
		if err := users.Create(context.Background(), u); err != nil {
			t.Fatalf("create %s: %v", email, err)
		}
		*id = u.ID
	}
	return f
}

func mustNewJob(t *testing.T, owner, title string) *job.Job {
	t.Helper()
	j, err := job.New(title)
	if err != nil {
		t.Fatalf("job.New(%q): %v", title, err)
	}
	j.UserID = owner
	return j
}

func titles(jobs []*job.Job) []string {
	out := make([]string, len(jobs))
	for i, j := range jobs {
		out[i] = j.Title
	}
	return out
}

func ids(jobs []*job.Job) []string {
	out := make([]string, len(jobs))
	for i, j := range jobs {
		out[i] = j.ID
	}
	return out
}

func TestJobStoreAddAndGetRoundTrip(t *testing.T) {
	f := newJobsFixture(t)
	ctx := context.Background()
	want := mustNewJob(t, f.alice, "send welcome email")

	if err := f.s.Add(ctx, want); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, err := f.s.Get(ctx, f.alice, want.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if *got != *want {
		t.Errorf("round trip changed the job:\n got  %+v\n want %+v", got, want)
	}
	if got.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt location = %v, want UTC", got.CreatedAt.Location())
	}
}

func TestJobStoreGetMissing(t *testing.T) {
	f := newJobsFixture(t)

	_, err := f.s.Get(context.Background(), f.alice, "does-not-exist")
	if !errors.Is(err, job.ErrNotFound) {
		t.Errorf("err = %v, want job.ErrNotFound", err)
	}
}

func TestJobStoreAddRejectsBadInput(t *testing.T) {
	f := newJobsFixture(t)
	ctx := context.Background()
	j := mustNewJob(t, f.alice, "send email")

	if err := f.s.Add(ctx, j); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if err := f.s.Add(ctx, j); !errors.Is(err, job.ErrDuplicateID) {
		t.Errorf("second Add err = %v, want job.ErrDuplicateID", err)
	}
	if err := f.s.Add(ctx, nil); !errors.Is(err, job.ErrNilJob) {
		t.Errorf("Add(nil) err = %v, want job.ErrNilJob", err)
	}
	if err := f.s.Add(ctx, mustNewJob(t, "", "no owner")); !errors.Is(err, job.ErrNoOwner) {
		t.Errorf("Add(no owner) err = %v, want job.ErrNoOwner", err)
	}
	// An owner that isn't a real user is refused by the foreign key.
	if err := f.s.Add(ctx, mustNewJob(t, "ffffffffffffffffffffffffffffffff", "ghost")); err == nil {
		t.Error("Add with an unknown owner succeeded, want an error")
	}
}

// The core rule of Task 18: one user can never see, list or delete another's jobs.
func TestJobStoreKeepsUsersApart(t *testing.T) {
	f := newJobsFixture(t)
	ctx := context.Background()

	alicesJob := mustNewJob(t, f.alice, "alice's job")
	if err := f.s.Add(ctx, alicesJob); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Add(ctx, mustNewJob(t, f.bob, "bob's job")); err != nil {
		t.Fatal(err)
	}

	if _, err := f.s.Get(ctx, f.bob, alicesJob.ID); !errors.Is(err, job.ErrNotFound) {
		t.Errorf("bob reading alice's job: err = %v, want job.ErrNotFound", err)
	}
	if err := f.s.Delete(ctx, f.bob, alicesJob.ID); !errors.Is(err, job.ErrNotFound) {
		t.Errorf("bob deleting alice's job: err = %v, want job.ErrNotFound", err)
	}
	if _, err := f.s.Get(ctx, f.alice, alicesJob.ID); err != nil {
		t.Errorf("alice's job is gone after bob's delete attempt: %v", err)
	}

	page, err := f.s.List(ctx, f.bob, job.ListParams{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := titles(page.Jobs); !slices.Equal(got, []string{"bob's job"}) {
		t.Errorf("bob's list = %v, want only his own job", got)
	}
}

func TestDeletingUserDeletesTheirJobs(t *testing.T) {
	f := newJobsFixture(t)
	ctx := context.Background()
	j := mustNewJob(t, f.alice, "alice's job")
	if err := f.s.Add(ctx, j); err != nil {
		t.Fatal(err)
	}

	if _, err := f.db.ExecContext(ctx, "DELETE FROM users WHERE id = $1", f.alice); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	var n int
	if err := f.db.QueryRowContext(ctx, "SELECT count(*) FROM jobs WHERE id = $1", j.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("job outlived its owner")
	}
}

func TestJobStoreList(t *testing.T) {
	f := newJobsFixture(t)
	ctx := context.Background()

	empty, err := f.s.List(ctx, f.alice, job.ListParams{Limit: 10})
	if err != nil {
		t.Fatalf("List on empty table: %v", err)
	}
	if empty.Jobs == nil || len(empty.Jobs) != 0 || empty.Next != nil {
		t.Errorf("List on empty table = %+v, want an empty non-nil slice and no cursor", empty)
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	inputs := []struct {
		title  string
		offset time.Duration
		status job.Status
	}{
		{"third", 2 * time.Minute, job.StatusPending},
		{"first", 0, job.StatusCompleted},
		{"second", time.Minute, job.StatusPending},
	}
	for _, in := range inputs {
		j := mustNewJob(t, f.alice, in.title)
		j.CreatedAt = base.Add(in.offset)
		j.UpdatedAt = j.CreatedAt
		j.Status = in.status
		if err := f.s.Add(ctx, j); err != nil {
			t.Fatalf("Add(%q): %v", in.title, err)
		}
	}

	all, err := f.s.List(ctx, f.alice, job.ListParams{Limit: 10})
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if got, want := titles(all.Jobs), []string{"first", "second", "third"}; !slices.Equal(got, want) {
		t.Errorf("List all = %v, want %v", got, want)
	}

	pending, err := f.s.List(ctx, f.alice, job.ListParams{Status: job.StatusPending, Limit: 10})
	if err != nil {
		t.Fatalf("List pending: %v", err)
	}
	if got, want := titles(pending.Jobs), []string{"second", "third"}; !slices.Equal(got, want) {
		t.Errorf("List pending = %v, want %v", got, want)
	}
}

func TestJobStoreListPaginates(t *testing.T) {
	f := newJobsFixture(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Five jobs. The last two share a created_at, so the ID has to break the tie.
	offsets := []time.Duration{0, time.Minute, 2 * time.Minute, 3 * time.Minute, 3 * time.Minute}
	for i, off := range offsets {
		j := mustNewJob(t, f.alice, fmt.Sprintf("job %d", i))
		j.CreatedAt = base.Add(off)
		j.UpdatedAt = j.CreatedAt
		if err := f.s.Add(ctx, j); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	everything, err := f.s.List(ctx, f.alice, job.ListParams{Limit: job.MaxPageSize})
	if err != nil {
		t.Fatalf("List everything: %v", err)
	}
	want := ids(everything.Jobs)
	if len(want) != len(offsets) {
		t.Fatalf("stored %d jobs, want %d", len(want), len(offsets))
	}

	var got []string
	var after *job.Cursor
	for pages := 1; ; pages++ {
		page, err := f.s.List(ctx, f.alice, job.ListParams{After: after, Limit: 2})
		if err != nil {
			t.Fatalf("List page %d: %v", pages, err)
		}
		got = append(got, ids(page.Jobs)...)
		if page.Next == nil {
			if pages != 3 {
				t.Errorf("pages = %d, want 3 (2 + 2 + 1)", pages)
			}
			break
		}
		if pages > 100 {
			t.Fatal("pagination never reached a last page")
		}
		after = page.Next
	}

	if !slices.Equal(got, want) {
		t.Errorf("paged IDs = %v,\nwant %v", got, want)
	}
}

func TestJobStoreListRejectsBadLimit(t *testing.T) {
	f := newJobsFixture(t)

	_, err := f.s.List(context.Background(), f.alice, job.ListParams{Limit: 0})
	if !errors.Is(err, job.ErrInvalidLimit) {
		t.Errorf("err = %v, want job.ErrInvalidLimit", err)
	}
}

func TestJobStoreDelete(t *testing.T) {
	f := newJobsFixture(t)
	ctx := context.Background()
	j := mustNewJob(t, f.alice, "send email")

	if err := f.s.Add(ctx, j); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := f.s.Delete(ctx, f.alice, j.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := f.s.Get(ctx, f.alice, j.ID); !errors.Is(err, job.ErrNotFound) {
		t.Errorf("Get after Delete err = %v, want job.ErrNotFound", err)
	}
	if err := f.s.Delete(ctx, f.alice, j.ID); !errors.Is(err, job.ErrNotFound) {
		t.Errorf("second Delete err = %v, want job.ErrNotFound", err)
	}
}

func TestJobStoreStopsWhenContextIsCancelled(t *testing.T) {
	f := newJobsFixture(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := f.s.Get(ctx, f.alice, "anything")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestJobStoreConcurrentAdds(t *testing.T) {
	f := newJobsFixture(t)
	ctx := context.Background()
	const n = 50

	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, err := job.New(fmt.Sprintf("job %d", i))
			if err != nil {
				t.Errorf("job.New: %v", err)
				return
			}
			j.UserID = f.alice
			if err := f.s.Add(ctx, j); err != nil {
				t.Errorf("Add: %v", err)
			}
		}()
	}
	wg.Wait()

	all, err := f.s.List(ctx, f.alice, job.ListParams{Limit: job.MaxPageSize})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all.Jobs) != n {
		t.Errorf("stored %d jobs, want %d", len(all.Jobs), n)
	}
}

// Migration 0004 deletes jobs that existed before owners did. This test builds
// a database at version 3, adds an ownerless job, then runs every migration.
func TestMigration4DeletesOwnerlessJobs(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	upTo3 := fstest.MapFS{}
	for _, name := range []string{"0001_create_jobs.sql", "0002_create_users.sql", "0003_create_sessions.sql"} {
		body, err := fs.ReadFile(migrationFiles, "migrations/"+name)
		if err != nil {
			t.Fatal(err)
		}
		upTo3["migrations/"+name] = &fstest.MapFile{Data: body}
	}
	if _, err := migrate(ctx, db, upTo3); err != nil {
		t.Fatalf("migrate to version 3: %v", err)
	}
	now := time.Now().UTC()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO jobs (id, title, status, created_at, updated_at) VALUES ($1, 'old', 'pending', $2, $2)`,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", now); err != nil {
		t.Fatalf("insert ownerless job: %v", err)
	}

	applied, err := Migrate(ctx, db)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if !slices.Contains(applied, 4) {
		t.Fatalf("applied %v, want it to include 4", applied)
	}
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM jobs").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d ownerless jobs survived migration 4", n)
	}
}
