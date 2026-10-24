package database

import (
	"context"
	"os"
	"testing"
	"time"
)

// testDatabaseURL returns the test database URL, or skips the test if it isn't set.
func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("GOFLOW_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("GOFLOW_TEST_DATABASE_URL not set; skipping test that needs PostgreSQL")
	}
	return url
}

func TestOpenConnectsAndConfiguresPool(t *testing.T) {
	db, err := Open(context.Background(), testDatabaseURL(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	var one int
	if err := db.QueryRowContext(context.Background(), "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("SELECT 1: %v", err)
	}
	if one != 1 {
		t.Errorf("SELECT 1 returned %d", one)
	}

	if got := db.Stats().MaxOpenConnections; got != 10 {
		t.Errorf("MaxOpenConnections = %d, want 10", got)
	}
}

func TestOpenFailsFastWhenDatabaseIsUnreachable(t *testing.T) {
	// Port 1 has nothing listening, so the connection is refused immediately.
	url := "postgres://goflow:goflow@127.0.0.1:1/goflow?sslmode=disable"

	start := time.Now()
	db, err := Open(context.Background(), url)
	if err == nil {
		db.Close()
		t.Fatal("Open succeeded against an unreachable database, want an error")
	}
	if elapsed := time.Since(start); elapsed > 6*time.Second {
		t.Errorf("Open took %v, want it to give up within the 5s ping timeout", elapsed)
	}
}
