package database

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func embeddedMigrationCount(t *testing.T) int {
	t.Helper()
	ms, err := loadMigrations(migrationFiles)
	if err != nil {
		t.Fatalf("load embedded migrations: %v", err)
	}
	return len(ms)
}

func TestMigrateCreatesSchema(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	applied, err := Migrate(ctx, db)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if want := embeddedMigrationCount(t); len(applied) != want {
		t.Errorf("applied %v, want %d migrations", applied, want)
	}

	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM jobs").Scan(&n); err != nil {
		t.Fatalf("query jobs table: %v", err)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if _, err := Migrate(ctx, db); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	applied, err := Migrate(ctx, db)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("second run applied %v, want nothing", applied)
	}
}

func TestMigrateConcurrently(t *testing.T) {
	db := newTestDB(t)
	const callers = 5

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		total int
	)
	errs := make(chan error, callers)

	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			applied, err := Migrate(context.Background(), db)
			if err != nil {
				errs <- err
				return
			}
			mu.Lock()
			total += len(applied)
			mu.Unlock()
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent Migrate: %v", err)
	}
	if want := embeddedMigrationCount(t); total != want {
		t.Errorf("migrations applied %d times in total, want %d (each exactly once)", total, want)
	}
}

func TestJobsTableConstraints(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if _, err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	const insert = `INSERT INTO jobs (id, title, status, created_at, updated_at) VALUES ($1, $2, $3, $4, $5)`
	now := time.Now().UTC()
	existingID := strings.Repeat("a", 32)

	if _, err := db.ExecContext(ctx, insert, existingID, "send email", "pending", now, now); err != nil {
		t.Fatalf("valid insert: %v", err)
	}

	// SQLSTATE codes: 23505 = unique_violation, 23514 = check_violation.
	tests := []struct {
		name             string
		id, title        string
		status           string
		created, updated time.Time
		wantCode         string
	}{
		{"duplicate id", existingID, "x", "pending", now, now, "23505"},
		{"id not 32 hex chars", "not-hex", "x", "pending", now, now, "23514"},
		{"empty title", strings.Repeat("b", 32), "", "pending", now, now, "23514"},
		{"title over 200 characters", strings.Repeat("c", 32), strings.Repeat("t", 201), "pending", now, now, "23514"},
		{"unknown status", strings.Repeat("d", 32), "x", "banana", now, now, "23514"},
		{"updated before created", strings.Repeat("e", 32), "x", "pending", now, now.Add(-time.Hour), "23514"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, insert, tc.id, tc.title, tc.status, tc.created, tc.updated)

			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatalf("err = %v, want a PostgreSQL error with code %s", err, tc.wantCode)
			}
			if pgErr.Code != tc.wantCode {
				t.Errorf("code = %s (%s), want %s", pgErr.Code, pgErr.Message, tc.wantCode)
			}
		})
	}
}

func TestLoadMigrationsSortsByVersionNumber(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/10_ten.sql": {Data: []byte("SELECT 10")},
		"migrations/9_nine.sql": {Data: []byte("SELECT 9")},
		"migrations/README.md":  {Data: []byte("not a migration")},
	}

	ms, err := loadMigrations(fsys)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(ms) != 2 || ms[0].version != 9 || ms[1].version != 10 {
		t.Errorf("got versions %v, want [9 10]", versions(ms))
	}
}

func TestLoadMigrationsRejectsBadFiles(t *testing.T) {
	tests := map[string]fstest.MapFS{
		"no version number": {"migrations/create_jobs.sql": {Data: []byte("x")}},
		"no underscore":     {"migrations/0001.sql": {Data: []byte("x")}},
		"version zero":      {"migrations/0_init.sql": {Data: []byte("x")}},
		"duplicate version": {
			"migrations/0001_a.sql": {Data: []byte("x")},
			"migrations/0001_b.sql": {Data: []byte("y")},
		},
	}

	for name, fsys := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := loadMigrations(fsys); err == nil {
				t.Error("loadMigrations succeeded, want an error")
			}
		})
	}
}

func versions(ms []migration) []int {
	out := make([]int, len(ms))
	for i, m := range ms {
		out[i] = m.version
	}
	return out
}
