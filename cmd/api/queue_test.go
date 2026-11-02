package main

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/work-frame/goflow/internal/queue"
)

// A job created through the API ends up in Redis, by ID.
func TestCreatedJobsReachTheQueue(t *testing.T) {
	dbURL := testDatabaseURL(t)
	redisURL := testRedisURL(t)

	cfg := testConfig("127.0.0.1:0", dbURL)
	cfg.RedisURL = redisURL
	base, _ := startServerWith(t, cfg)
	client := &http.Client{Timeout: 5 * time.Second}

	if status, _, body := request(t, client, http.MethodGet, base+"/readyz", ""); status != http.StatusOK {
		t.Fatalf("readyz with a queue: status %d, body %s", status, body)
	}

	token := signUp(t, client, base, dbURL)
	id := createJob(t, client, base, token, "queued e2e job")

	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opts)
	ctx := context.Background()
	// Cleanups run last-registered first: remove this test's ID (other IDs
	// in the list are left alone), then close the client.
	t.Cleanup(func() { rdb.Close() })
	t.Cleanup(func() { rdb.LRem(ctx, queue.PendingKey, 0, id) })

	ids, err := rdb.LRange(ctx, queue.PendingKey, 0, -1).Result()
	if err != nil {
		t.Fatalf("LRANGE: %v", err)
	}
	if !slices.Contains(ids, id) {
		t.Fatalf("job %s not in %s: %v", id, queue.PendingKey, ids)
	}
}
