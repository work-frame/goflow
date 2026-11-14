package database

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// The database benchmarks (Task 57). Each run of a benchmark gets its own
// copy of the migrated template, dropped when the run ends, so no run
// sees another's jobs or outbox rows. Run them with scripts/bench.sh.

// benchOwner creates the user every benchmark job belongs to.
func benchOwner(b *testing.B, db *sql.DB) string {
	b.Helper()
	u := mustNewUser(b, "bench@example.com")
	if err := NewUserStore(db).Create(context.Background(), u); err != nil {
		b.Fatal(err)
	}
	return u.ID
}

// insertPending inserts n pending jobs in one statement and returns their
// IDs: setup, not what is measured.
func insertPending(b *testing.B, db *sql.DB, owner string, n int) []string {
	b.Helper()
	rows, err := db.QueryContext(context.Background(),
		`INSERT INTO jobs (id, title, status, created_at, updated_at, user_id)
		 SELECT md5(random()::text || g::text), 'bench', 'pending', now(), now(), $1
		 FROM generate_series(1, $2) AS g
		 RETURNING id`, owner, n)
	if err != nil {
		b.Fatal(err)
	}
	defer rows.Close()
	ids := make([]string, 0, n)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			b.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		b.Fatal(err)
	}
	if len(ids) != n {
		b.Fatalf("inserted %d jobs, want %d", len(ids), n)
	}
	return ids
}

// One job's two database writes as a worker makes them: Claim (pending →
// processing, under a new lease) then Complete (fenced by that lease). The
// result is the cost of the pair together, round trips included; it is
// not the cost of either one alone.
//
// Every Claim must win and every Complete must be accepted. A refused
// write fails the benchmark: timing operations that didn't happen would
// report a speed GoFlow doesn't have (FAILURES.md, case 54).
func BenchmarkClaimAndComplete(b *testing.B) {
	db := newMigratedTestDB(b)
	ids := insertPending(b, db, benchOwner(b, db), b.N)
	work := NewWorkStore(db)
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c, ok, err := work.Claim(ctx, ids[i], 30*time.Second)
		if err != nil || !ok {
			b.Fatalf("claim %d of %d: won %v, err %v", i+1, b.N, ok, err)
		}
		if err := work.Complete(ctx, ids[i], c.LeaseToken); err != nil {
			b.Fatalf("complete %d of %d refused: %v (a benchmark of writes that didn't happen measures nothing)", i+1, b.N, err)
		}
	}
}

// One relay pass over exactly 100 outbox rows: lock them, hand their IDs
// to Redis, delete them and mark the jobs queued, in one transaction.
// Redis is a fake that accepts at once, so this is the database's side
// only. The rows are made before each pass and the jobs deleted after it,
// with the clock stopped, so every pass starts from the same 100 rows and
// nothing piles up.
func BenchmarkRelayPass(b *testing.B) {
	const batch = 100
	db := newMigratedTestDB(b)
	owner := benchOwner(b, db)
	outbox := NewOutboxStore(db)
	ctx := context.Background()
	var sent int
	send := func(_ context.Context, ids []string) error {
		sent += len(ids)
		return nil
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		ids := insertPending(b, db, owner, batch)
		if _, err := db.ExecContext(ctx, `INSERT INTO job_outbox (job_id, created_at) SELECT unnest($1::text[]), now()`, ids); err != nil {
			b.Fatal(err)
		}
		sent = 0
		b.StartTimer()

		n, err := outbox.Relay(ctx, batch, send)

		b.StopTimer()
		if err != nil || n != batch || sent != batch {
			b.Fatalf("pass %d: relayed %d, sent %d, err %v; want %d and %d", i+1, n, sent, err, batch, batch)
		}
		var left int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM job_outbox`).Scan(&left); err != nil || left != 0 {
			b.Fatalf("pass %d: %d outbox rows left (err %v), want 0", i+1, left, err)
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM jobs WHERE id = ANY($1)`, ids); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
}
