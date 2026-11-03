package queue

import (
	"context"
	"errors"
	"os"
	"slices"
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
	q.key = "goflow:test:" + t.Name() + ":pending"
	q.processing = "goflow:test:" + t.Name() + ":processing"
	q.rdb.Del(context.Background(), q.key, q.processing)
	t.Cleanup(func() {
		q.rdb.Del(context.Background(), q.key, q.processing)
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

func processingIDs(t *testing.T, q *Queue) []string {
	t.Helper()
	ids, err := q.rdb.LRange(context.Background(), q.processing, 0, -1).Result()
	if err != nil {
		t.Fatalf("LRANGE processing: %v", err)
	}
	return ids
}

// Take moves the ID into the processing list; only Ack removes it.
func TestTakeKeepsTheIDUntilAck(t *testing.T) {
	q := openTestQueue(t)
	ctx := context.Background()
	if err := q.Enqueue(ctx, "older", "newer"); err != nil {
		t.Fatal(err)
	}

	id, err := q.Take(ctx, time.Second)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if id != "older" {
		t.Fatalf("Take = %q, want the oldest ID", id)
	}
	if got := processingIDs(t, q); len(got) != 1 || got[0] != "older" {
		t.Fatalf("processing list = %v, want [older]: a taken ID must not exist only in memory", got)
	}

	if err := q.Ack(ctx, "older"); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if got := processingIDs(t, q); len(got) != 0 {
		t.Fatalf("processing list after Ack = %v, want empty", got)
	}
}

func TestTakeWaitsThenReportsEmpty(t *testing.T) {
	q := openTestQueue(t)

	start := time.Now()
	_, err := q.Take(context.Background(), time.Second)
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("Take on an empty queue: err = %v, want ErrEmpty", err)
	}
	if waited := time.Since(start); waited < 900*time.Millisecond {
		t.Errorf("Take returned after %v, want it to wait about 1s", waited)
	}
}

func TestRecoverPutsUnfinishedIDsFirst(t *testing.T) {
	q := openTestQueue(t)
	ctx := context.Background()
	if err := q.Enqueue(ctx, "a", "b", "c"); err != nil {
		t.Fatal(err)
	}
	for range 2 { // a worker takes a and b, then dies without Ack
		if _, err := q.Take(ctx, time.Second); err != nil {
			t.Fatal(err)
		}
	}

	n, err := q.Recover(ctx)
	if err != nil || n != 2 {
		t.Fatalf("Recover = %d, %v; want 2, nil", n, err)
	}
	var order []string
	for range 3 {
		id, err := q.Take(ctx, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		order = append(order, id)
	}
	if want := []string{"a", "b", "c"}; !slices.Equal(order, want) {
		t.Fatalf("order after Recover = %v, want %v", order, want)
	}
}

// New doesn't connect, so it succeeds with nothing listening; the first
// command is what fails.
func TestNewDoesNotConnect(t *testing.T) {
	q, err := New("redis://127.0.0.1:1/0")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer q.Close()
	if err := q.PingContext(context.Background()); err == nil {
		t.Fatal("Ping succeeded with nothing listening on port 1")
	}
	if _, err := New("http://localhost:6379"); err == nil {
		t.Fatal("New accepted an http:// URL")
	}
}
