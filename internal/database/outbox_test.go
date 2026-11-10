package database

import (
	"context"
	"errors"
	"io/fs"
	"slices"
	"testing"
	"testing/fstest"
	"time"

	"github.com/work-frame/goflow/internal/job"
)

// addJobs adds titles as alice's jobs, one at a time, and returns their IDs
// in creation order.
func addJobs(t *testing.T, f *jobsFixture, titles ...string) []string {
	t.Helper()
	var out []string
	for _, title := range titles {
		j := mustNewJob(t, f.alice, title)
		if err := f.s.Add(context.Background(), j); err != nil {
			t.Fatalf("Add(%q): %v", title, err)
		}
		out = append(out, j.ID)
	}
	return out
}

func waiting(t *testing.T, o *OutboxStore) int64 {
	t.Helper()
	n, err := o.Waiting(context.Background())
	if err != nil {
		t.Fatalf("Waiting: %v", err)
	}
	return n
}

func statusOf(t *testing.T, f *jobsFixture, id string) job.Status {
	t.Helper()
	j, err := f.s.Get(context.Background(), f.alice, id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return j.Status
}

func TestAddWritesAnOutboxRow(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	o := NewOutboxStore(f.db)
	id := addJobs(t, f, "send welcome email")[0]

	var got string
	if err := f.db.QueryRowContext(context.Background(), `SELECT job_id FROM job_outbox`).Scan(&got); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	if got != id || waiting(t, o) != 1 {
		t.Fatalf("outbox holds %q (%d rows), want exactly %q", got, waiting(t, o), id)
	}
}

// The job and its outbox row are written together or not at all. Renaming
// the outbox table makes the second insert fail after the first succeeded.
func TestAddIsAllOrNothing(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	ctx := context.Background()
	if _, err := f.db.ExecContext(ctx, `ALTER TABLE job_outbox RENAME TO job_outbox_gone`); err != nil {
		t.Fatal(err)
	}

	j := mustNewJob(t, f.alice, "half written")
	if err := f.s.Add(ctx, j); err == nil {
		t.Fatal("Add succeeded without an outbox table")
	}
	if _, err := f.s.Get(ctx, f.alice, j.ID); !errors.Is(err, job.ErrNotFound) {
		t.Fatalf("Get after the failed Add: err = %v, want job.ErrNotFound: the job was saved without its outbox row", err)
	}
}

func TestRelaySendsOldestFirstThenClears(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	o := NewOutboxStore(f.db)
	want := addJobs(t, f, "first", "second", "third")

	var sent []string
	n, err := o.Relay(context.Background(), 100, func(_ context.Context, ids []string) error {
		sent = append(sent, ids...)
		return nil
	})
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	if n != 3 || !slices.Equal(sent, want) {
		t.Fatalf("Relay sent %d: %v, want %v", n, sent, want)
	}
	if w := waiting(t, o); w != 0 {
		t.Errorf("%d rows still waiting after a successful send", w)
	}
	for _, id := range want {
		if s := statusOf(t, f, id); s != job.StatusQueued {
			t.Errorf("job %s status = %s, want queued", id, s)
		}
	}
}

func TestRelayKeepsRowsWhenSendFails(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	o := NewOutboxStore(f.db)
	ids := addJobs(t, f, "one", "two")

	redisDown := errors.New("redis: connection refused")
	n, err := o.Relay(context.Background(), 100, func(context.Context, []string) error { return redisDown })
	if !errors.Is(err, redisDown) || n != 0 {
		t.Fatalf("Relay = %d, %v; want 0, the send error", n, err)
	}
	if w := waiting(t, o); w != 2 {
		t.Errorf("%d rows waiting after a failed send, want 2: they must be retried", w)
	}
	for _, id := range ids {
		if s := statusOf(t, f, id); s != job.StatusPending {
			t.Errorf("job %s status = %s, want pending", id, s)
		}
	}
}

func TestRelayTakesAtMostLimit(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	o := NewOutboxStore(f.db)
	ids := addJobs(t, f, "a", "b", "c")
	ctx := context.Background()

	var batches [][]string
	send := func(_ context.Context, b []string) error { batches = append(batches, b); return nil }
	for range 2 {
		if _, err := o.Relay(ctx, 2, send); err != nil {
			t.Fatal(err)
		}
	}
	if len(batches) != 2 || !slices.Equal(batches[0], ids[:2]) || !slices.Equal(batches[1], ids[2:]) {
		t.Fatalf("batches = %v, want %v then %v", batches, ids[:2], ids[2:])
	}
}

// While one relay holds rows (its send is still talking to Redis), a second
// relay skips them instead of sending the same IDs again.
func TestRelaySkipsRowsAnotherRelayHolds(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	o := NewOutboxStore(f.db)
	ids := addJobs(t, f, "held", "free")
	ctx := context.Background()

	inSend := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, err := o.Relay(ctx, 1, func(context.Context, []string) error {
			close(inSend)
			<-release
			return nil
		})
		firstDone <- err
	}()
	<-inSend

	var sent []string
	if _, err := o.Relay(ctx, 100, func(_ context.Context, b []string) error { sent = b; return nil }); err != nil {
		t.Fatalf("second Relay: %v", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Relay: %v", err)
	}

	if !slices.Equal(sent, ids[1:]) {
		t.Fatalf("second relay sent %v, want only %v", sent, ids[1:])
	}
}

func TestDeletingAJobRemovesItsOutboxRow(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	o := NewOutboxStore(f.db)
	id := addJobs(t, f, "changed my mind")[0]

	if err := f.s.Delete(context.Background(), f.alice, id); err != nil {
		t.Fatal(err)
	}
	if w := waiting(t, o); w != 0 {
		t.Errorf("%d rows waiting for a deleted job", w)
	}
}

// Jobs created before the outbox existed get a row, so they are queued too.
func TestMigration6QueuesExistingPendingJobs(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	ctx := context.Background()

	upTo5 := fstest.MapFS{}
	matches, err := fs.Glob(migrationFiles, "migrations/000[1-5]_*.sql")
	if err != nil || len(matches) != 5 {
		t.Fatalf("migrations 1-5: %v, %v", matches, err)
	}
	for _, name := range matches {
		body, err := fs.ReadFile(migrationFiles, name)
		if err != nil {
			t.Fatal(err)
		}
		upTo5[name] = &fstest.MapFile{Data: body}
	}
	if _, err := migrate(ctx, db, upTo5); err != nil {
		t.Fatalf("migrate to version 5: %v", err)
	}

	u := mustNewUser(t, "old@example.com")
	if err := NewUserStore(db).Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	for id, status := range map[string]string{
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": "pending",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb": "completed",
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO jobs (id, user_id, title, status, created_at, updated_at) VALUES ($1, $2, 'old', $3, $4, $4)`,
			id, u.ID, status, now); err != nil {
			t.Fatalf("insert %s job: %v", status, err)
		}
	}

	applied, err := Migrate(ctx, db)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if !slices.Contains(applied, 6) {
		t.Fatalf("applied %v, want it to include 6", applied)
	}
	var got []string
	rows, err := db.QueryContext(ctx, `SELECT job_id FROM job_outbox`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if !slices.Equal(got, []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}) {
		t.Fatalf("outbox after migration 6 = %v, want only the pending job", got)
	}
}

// relayAll moves every outbox row to "queued", as the relay does.
func relayAll(t *testing.T, o *OutboxStore) {
	t.Helper()
	if _, err := o.Relay(context.Background(), 100, func(context.Context, []string) error { return nil }); err != nil {
		t.Fatalf("Relay: %v", err)
	}
}

func TestRequeueStaleResendsOnlyOldQueuedJobs(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	o := NewOutboxStore(f.db)
	ctx := context.Background()
	ids := addJobs(t, f, "stuck", "fresh", "still pending")
	relayAll(t, o) // all three queued
	if _, err := f.db.ExecContext(ctx, `UPDATE jobs SET status = 'pending' WHERE id = $1`, ids[2]); err != nil {
		t.Fatal(err)
	}
	// "stuck" was queued an hour ago; the others just now.
	if _, err := f.db.ExecContext(ctx,
		`UPDATE jobs SET created_at = now() - interval '2 hours', updated_at = now() - interval '1 hour' WHERE id = $1`, ids[0]); err != nil {
		t.Fatal(err)
	}

	var sent []string
	send := func(_ context.Context, b []string) error { sent = append(sent, b...); return nil }
	n, err := o.RequeueStale(ctx, 10*time.Minute, 100, send)
	if err != nil {
		t.Fatalf("RequeueStale: %v", err)
	}
	if n != 1 || !slices.Equal(sent, ids[:1]) {
		t.Fatalf("RequeueStale sent %d: %v, want only %v", n, sent, ids[:1])
	}

	// Its updated_at moved forward, so it isn't sent again right away.
	sent = nil
	if n, err := o.RequeueStale(ctx, 10*time.Minute, 100, send); err != nil || n != 0 {
		t.Fatalf("second RequeueStale = %d (%v), %v; want 0: a re-sent job must wait another 10 minutes", n, sent, err)
	}
	if s := statusOf(t, f, ids[0]); s != job.StatusQueued {
		t.Errorf("status after re-send = %s, want still queued", s)
	}
}

func TestRequeueStaleKeepsJobsStaleWhenSendFails(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	o := NewOutboxStore(f.db)
	ctx := context.Background()
	id := addJobs(t, f, "stuck")[0]
	relayAll(t, o)
	if _, err := f.db.ExecContext(ctx,
		`UPDATE jobs SET created_at = now() - interval '2 hours', updated_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}

	down := errors.New("redis: connection refused")
	if _, err := o.RequeueStale(ctx, 10*time.Minute, 100, func(context.Context, []string) error { return down }); !errors.Is(err, down) {
		t.Fatalf("RequeueStale err = %v, want the send error", err)
	}
	n, err := o.RequeueStale(ctx, 10*time.Minute, 100, func(context.Context, []string) error { return nil })
	if err != nil || n != 1 {
		t.Fatalf("after a failed send, RequeueStale = %d, %v; want 1: the job must still count as stale", n, err)
	}
}
