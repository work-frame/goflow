package queue

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// openTestQueue connects to the Redis in GOFLOW_TEST_REDIS_URL and gives the
// test its own list, deleted at the end, so tests never touch PendingKey
// and can't see each other's IDs.
func openTestQueue(t *testing.T) *Queue {
	t.Helper()
	url := os.Getenv("GOFLOW_TEST_REDIS_URL")
	if url == "" {
		t.Skip("GOFLOW_TEST_REDIS_URL not set; skipping test that needs Redis")
	}
	q, err := Open(context.Background(), url)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	q.key = "goflow:test:" + t.Name()
	q.rdb.Del(context.Background(), q.key)
	t.Cleanup(func() {
		q.rdb.Del(context.Background(), q.key)
		q.Close()
	})
	return q
}

func TestEnqueueKeepsOrder(t *testing.T) {
	q := openTestQueue(t)
	ctx := context.Background()

	if err := q.Enqueue(ctx, "first", "second"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := q.Enqueue(ctx, "third"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := q.Enqueue(ctx); err != nil {
		t.Fatalf("Enqueue with no IDs: %v", err)
	}

	n, err := q.Len(ctx)
	if err != nil {
		t.Fatalf("Len: %v", err)
	}
	if n != 3 {
		t.Fatalf("Len = %d, want 3", n)
	}

	// Workers will pop from the right (RPOP), so the oldest ID must be there.
	got, err := q.rdb.RPop(ctx, q.key).Result()
	if err != nil {
		t.Fatalf("RPop: %v", err)
	}
	if got != "first" {
		t.Errorf("oldest ID = %q, want first", got)
	}
	if got, _ := q.rdb.RPop(ctx, q.key).Result(); got != "second" {
		t.Errorf("next ID = %q, want second: one call must keep its IDs in order", got)
	}
}

func TestOpenFailsFastWhenRedisIsDown(t *testing.T) {
	// Port 1 on this machine: nothing listens there, so the dial is refused.
	start := time.Now()
	_, err := Open(context.Background(), "redis://:secret-redis-password@127.0.0.1:1/0")
	if err == nil {
		t.Fatal("Open succeeded with no Redis running")
	}
	if took := time.Since(start); took > 6*time.Second {
		t.Errorf("Open took %v to fail, want under 6s", took)
	}
	if strings.Contains(err.Error(), "secret-redis-password") {
		t.Errorf("error leaks the password: %v", err)
	}
}

func TestOpenRejectsBadURLsWithoutQuotingThem(t *testing.T) {
	for _, url := range []string{
		"http://:secret-redis-password@localhost:6379",
		"redis://:secret-redis-password@localhost:6379/not-a-db",
		"not a url secret-redis-password",
	} {
		_, err := Open(context.Background(), url)
		if err == nil {
			t.Errorf("Open(%q) succeeded, want an error", url)
			continue
		}
		if strings.Contains(err.Error(), "secret-redis-password") {
			t.Errorf("Open(%q): error leaks the password: %v", url, err)
		}
	}
}
