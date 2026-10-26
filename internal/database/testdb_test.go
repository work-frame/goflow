package database

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"testing"
)

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
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	return createTestDB(t, "")
}

// newMigratedTestDB returns a private copy of the migrated template,
// dropped when the test ends. Use it for anything that needs the schema.
func newMigratedTestDB(t *testing.T) *sql.DB {
	t.Helper()
	testDatabaseURL(t) // skip early if no test database is configured
	return createTestDB(t, templateName)
}

func createTestDB(t *testing.T, template string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	baseURL := testDatabaseURL(t)

	admin, err := Open(ctx, baseURL)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}

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
	t.Cleanup(func() { db.Close() }) // runs before the drop: cleanups are last-in-first-out
	return db
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
