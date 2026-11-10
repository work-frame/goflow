package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/work-frame/goflow/internal/database"
)

// Every test that uses PostgreSQL gets its own copy of one migrated
// template database, as in internal/database (Task 54). The servers of one
// test share it, like the processes of one deployment; different tests
// never do, so one test's relay, workers and lease reaper can't touch
// another's jobs.

// templateDatabase is the migrated template for this run. TestMain sets it
// before any test starts; tests only read it.
var templateDatabase string

// testDatabases remembers each test's copy, so every testDatabaseURL call
// in one test returns the same one.
var testDatabases sync.Map // *testing.T → URL

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

// runTests is separate from TestMain so its deferred cleanup runs: os.Exit
// skips defers written in TestMain itself.
func runTests(m *testing.M) int {
	baseURL := os.Getenv("GOFLOW_TEST_DATABASE_URL")
	if baseURL == "" {
		return m.Run() // tests that need PostgreSQL skip themselves
	}
	ctx := context.Background()
	admin, err := database.Open(ctx, baseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open test database: %v\n", err)
		return 1
	}
	defer admin.Close()
	admin.SetMaxOpenConns(1)

	name := randomDatabaseName("goflow_apitpl_")
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		fmt.Fprintf(os.Stderr, "create template database: %v\n", err)
		return 1
	}
	defer admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")

	tplURL, err := withDatabaseName(baseURL, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "template URL: %v\n", err)
		return 1
	}
	tpl, err := database.Open(ctx, tplURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open template database: %v\n", err)
		return 1
	}
	_, err = database.Migrate(ctx, tpl)
	tpl.Close() // PostgreSQL won't copy a database that has open connections
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate template database: %v\n", err)
		return 1
	}
	templateDatabase = name
	return m.Run()
}

// testDatabaseURL returns this test's own database, copied from the
// template on first use and dropped when the test ends, or skips the test
// without GOFLOW_TEST_DATABASE_URL.
func testDatabaseURL(t *testing.T) string {
	t.Helper()
	if u, ok := testDatabases.Load(t); ok {
		return u.(string)
	}
	u := newTestDatabaseURL(t)
	testDatabases.Store(t, u)
	t.Cleanup(func() { testDatabases.Delete(t) })
	return u
}

// newTestDatabaseURL returns a fresh copy every call: for a test that runs
// separate deployments side by side.
func newTestDatabaseURL(t *testing.T) string {
	t.Helper()
	baseURL := os.Getenv("GOFLOW_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("GOFLOW_TEST_DATABASE_URL not set; skipping test that needs PostgreSQL")
	}
	if templateDatabase == "" {
		t.Fatal("no template database: TestMain didn't run")
	}
	ctx := context.Background()
	admin, err := database.Open(ctx, baseURL)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	admin.SetMaxOpenConns(1) // one CREATE now, one DROP at the end

	// Both names are generated here, never taken from input, so building
	// the SQL with + is safe: identifiers can't be $1 parameters.
	name := randomDatabaseName("goflow_apitest_")
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name+" TEMPLATE "+templateDatabase); err != nil {
		admin.Close()
		t.Fatalf("copy the template database: %v", err)
	}
	// Registered first, so it runs after every server of the test has stopped.
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database %s: %v", name, err)
		}
		admin.Close()
	})
	u, err := withDatabaseName(baseURL, name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// databaseName is the database a URL points at: the name testConfig turns
// into the test's Redis key prefix and events channel.
func databaseName(dbURL string) string {
	u, err := url.Parse(dbURL)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Path, "/")
}

func withDatabaseName(baseURL, name string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	u.Path = "/" + name
	return u.String(), nil
}

func randomDatabaseName(prefix string) string {
	b := make([]byte, 6)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}
