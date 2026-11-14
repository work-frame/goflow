package database

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// testDBConns is how many connections one test's database copy may open.
// Tests run in parallel (Task 53), and each copy's pool counts against
// PostgreSQL's limit of 100, which cmd/api's tests share at the same time.
// A test that needs more at once asks for them with withConns; one that
// waits for a connection without asking fails (see createTestDB), because
// a race test whose callers queue for 3 connections no longer races.
const testDBConns = 3

// connWaitsExpected holds the pools of tests that make callers queue for
// connections on purpose (see expectConnWaits).
var connWaitsExpected sync.Map

// templateName is the migrated template database for this test run.
// TestMain sets it. It stays empty when no test database is configured.
var templateName string

// TestMain runs once per package, before any test in it.
func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

// runTests is separate from TestMain so its deferred cleanup actually runs:
// os.Exit would skip any defer written in TestMain itself.
func runTests(m *testing.M) int {
	baseURL := os.Getenv("GOFLOW_TEST_DATABASE_URL")
	if baseURL == "" {
		return m.Run() // tests that need PostgreSQL skip themselves
	}

	name, cleanup, err := createTemplate(baseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create template database: %v\n", err)
		return 1
	}
	defer cleanup()

	templateName = name
	return m.Run()
}

// createTemplate makes one migrated database that every test copies.
func createTemplate(baseURL string) (string, func(), error) {
	ctx := context.Background()

	admin, err := Open(ctx, baseURL)
	if err != nil {
		return "", nil, err
	}

	name := randomName("goflow_tpl_")
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close()
		return "", nil, err
	}
	cleanup := func() {
		admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		admin.Close()
	}

	tplURL, err := withDatabase(baseURL, name)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	tpl, err := Open(ctx, tplURL)
	if err != nil {
		cleanup()
		return "", nil, err
	}

	_, err = Migrate(ctx, tpl)
	// PostgreSQL refuses to copy a database that has open connections,
	// so close this pool now, whether or not Migrate succeeded.
	tpl.Close()
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return name, cleanup, nil
}

// newTestDB returns an empty database for one test, dropped when the test ends.
// Use it to test migrations themselves.
func newTestDB(t testing.TB) *sql.DB {
	t.Helper()
	return createTestDB(t, "")
}

// newMigratedTestDB returns a private copy of the migrated template,
// dropped when the test ends. Use it for anything that needs the schema.
func newMigratedTestDB(t testing.TB) *sql.DB {
	t.Helper()
	testDatabaseURL(t) // skip early if no test database is configured
	return createTestDB(t, templateName)
}

func createTestDB(t testing.TB, template string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	baseURL := testDatabaseURL(t)

	admin, err := Open(ctx, baseURL)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	admin.SetMaxOpenConns(1) // one CREATE now, one DROP at the end

	// Identifiers can't be $1 parameters. Both names are generated in this file,
	// never taken from user input, so building the SQL with + is safe here.
	name := randomName("goflow_test_")
	stmt := "CREATE DATABASE " + name
	if template != "" {
		stmt += " TEMPLATE " + template
	}
	if _, err := admin.ExecContext(ctx, stmt); err != nil {
		admin.Close()
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database %s: %v", name, err)
		}
		admin.Close()
	})

	dbURL, err := withDatabase(baseURL, name)
	if err != nil {
		t.Fatalf("build test database URL: %v", err)
	}
	db, err := Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	db.SetMaxOpenConns(testDBConns)
	db.SetMaxIdleConns(testDBConns)
	// Runs before the drop: cleanups are last-in-first-out.
	t.Cleanup(func() {
		st := db.Stats()
		db.Close()
		if _, ok := connWaitsExpected.Load(db); !ok && st.WaitCount > 0 {
			t.Errorf("this test waited %d times (%v in all) for one of its %d database connections: it needs more at once. Give it enough with withConns, or its concurrent callers queue instead of racing",
				st.WaitCount, st.WaitDuration.Round(time.Millisecond), st.MaxOpenConnections)
		}
	})
	return db
}

// withConns lets one test's database copy open n connections, for a test
// that runs n callers at once and means them to overlap.
func withConns(db *sql.DB, n int) {
	db.SetMaxOpenConns(n)
	db.SetMaxIdleConns(n)
}

// expectConnWaits marks a test whose callers queue for connections on
// purpose, so the check in createTestDB lets it wait.
func expectConnWaits(db *sql.DB) {
	connWaitsExpected.Store(db, true)
}

// withDatabase returns baseURL pointed at a different database name.
func withDatabase(baseURL, name string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	u.Path = "/" + name
	return u.String(), nil
}

func randomName(prefix string) string {
	b := make([]byte, 6)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// A test's cleanup drops its own copy and no other: with tests running in
// parallel, dropping another test's database would fail that test midway.
// The subtest's copy is gone once the subtest ends; this test's copy, made
// first and still in use, is untouched.
func TestCleanupDropsOnlyTheTestsOwnCopy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mine := newMigratedTestDB(t)
	var myName, theirName string
	if err := mine.QueryRowContext(ctx, "SELECT current_database()").Scan(&myName); err != nil {
		t.Fatal(err)
	}

	t.Run("other", func(t *testing.T) {
		theirs := newMigratedTestDB(t)
		if err := theirs.QueryRowContext(ctx, "SELECT current_database()").Scan(&theirName); err != nil {
			t.Fatal(err)
		}
	})

	if theirName == "" || theirName == myName || !strings.HasPrefix(theirName, "goflow_test_") {
		t.Fatalf("copies %q and %q: want two different goflow_test_ databases", myName, theirName)
	}
	var left int
	if err := mine.QueryRowContext(ctx, "SELECT count(*) FROM pg_database WHERE datname IN ($1, $2)", myName, theirName).Scan(&left); err != nil {
		t.Fatalf("this test's copy stopped working when the other's was dropped: %v", err)
	}
	if left != 1 {
		t.Fatalf("%d of the two copies left after the subtest, want 1 (this test's own)", left)
	}
	var users int
	if err := mine.QueryRowContext(ctx, "SELECT count(*) FROM users").Scan(&users); err != nil {
		t.Fatalf("this test's copy lost its schema: %v", err)
	}
}
