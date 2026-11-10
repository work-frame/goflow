package main

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/work-frame/goflow/internal/queue"
)

// A job created through the API reaches Redis through the outbox and the
// relay, and its status moves from pending to queued.
func TestCreatedJobsReachTheQueue(t *testing.T) {
	dbURL := testDatabaseURL(t)
	redisURL := testRedisURL(t)

	cfg := testConfig("127.0.0.1:0", dbURL)
	cfg.RedisURL = redisURL
	base, _ := startServerWith(t, cfg)
	client := testClient(t, 5*time.Second)

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

	// The relay runs in the background, woken by the create. It pushes the ID
	// to Redis first and commits "queued" a moment later, so wait for both.
	var ids []string
	var got struct {
		Status string `json:"status"`
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		ids, err = rdb.LRange(ctx, queue.PendingKey, 0, -1).Result()
		if err != nil {
			t.Fatalf("LRANGE: %v", err)
		}
		status, _, body := requestAs(t, client, http.MethodGet, base+"/api/v1/jobs/"+id, token, "")
		if status != http.StatusOK {
			t.Fatalf("get: status %d, body %s", status, body)
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(ids, id) && got.Status == "queued" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after 5s: job %s in %s = %v, status = %q; want the ID there and status queued",
				id, queue.PendingKey, slices.Contains(ids, id), got.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// With GOFLOW_WORKERS set, the API runs the job itself: outbox → relay →
// Redis → worker pool → completed, all in one process.
func TestAPIWorkersRunJobs(t *testing.T) {
	dbURL := testDatabaseURL(t)
	redisURL := testRedisURL(t)

	cfg := testConfig("127.0.0.1:0", dbURL) // placeholder work: 10 ms
	cfg.RedisURL = redisURL
	cfg.Workers = 2
	base, _ := startServerWith(t, cfg)
	client := testClient(t, 5*time.Second)

	token := signUp(t, client, base, dbURL)
	id := createJob(t, client, base, token, "run me in the API")

	var got struct {
		Status string `json:"status"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for got.Status != "completed" {
		if time.Now().After(deadline) {
			t.Fatalf("job %s status = %q after 10s, want completed", id, got.Status)
		}
		time.Sleep(20 * time.Millisecond)
		status, _, body := requestAs(t, client, http.MethodGet, base+"/api/v1/jobs/"+id, token, "")
		if status != http.StatusOK {
			t.Fatalf("get: status %d, body %s", status, body)
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
	}
}

// The API starts even when Redis isn't reachable: it serves requests, saves
// jobs to the outbox, and its relay and workers keep retrying in the
// background. Nothing listens on port 1.
func TestAPIStartsWhenRedisIsDown(t *testing.T) {
	dbURL := testDatabaseURL(t)

	cfg := testConfig("127.0.0.1:0", dbURL)
	cfg.RedisURL = "redis://127.0.0.1:1/0"
	cfg.Workers = 1
	base, _ := startServerWith(t, cfg)
	client := testClient(t, 10*time.Second)

	if status, _, body := request(t, client, http.MethodGet, base+"/readyz", ""); status != http.StatusOK {
		t.Fatalf("readyz with Redis down: status %d, body %s; readiness depends on the database only", status, body)
	}
	token := signUp(t, client, base, dbURL)
	id := createJob(t, client, base, token, "waits for redis")

	status, _, body := requestAs(t, client, http.MethodGet, base+"/api/v1/jobs/"+id, token, "")
	if status != http.StatusOK {
		t.Fatalf("get: status %d, body %s", status, body)
	}
	var got struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "pending" {
		t.Errorf("status = %q, want pending: with Redis down the job waits in the outbox", got.Status)
	}
}

// jobState reads a job's status and attempts through the API.
func jobState(t *testing.T, client *http.Client, base, token, id string) (string, int) {
	t.Helper()
	status, _, body := requestAs(t, client, http.MethodGet, base+"/api/v1/jobs/"+id, token, "")
	if status != http.StatusOK {
		t.Fatalf("get %s: status %d, body %s", id, status, body)
	}
	var got struct {
		Status   string `json:"status"`
		Attempts int    `json:"attempts"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	return got.Status, got.Attempts
}

// The whole retry path, in one process with real PostgreSQL and Redis:
// a job that fails once is retried after its backoff (delayed set →
// promoter → queue) and completes on attempt 2; a permanent failure is
// dead after 1 attempt.
func TestAPIRetriesAFailedJob(t *testing.T) {
	dbURL := testDatabaseURL(t)
	redisURL := testRedisURL(t)

	cfg := testConfig("127.0.0.1:0", dbURL)
	cfg.RedisURL = redisURL
	cfg.Workers = 2
	base, _ := startServerWith(t, cfg)
	client := testClient(t, 5*time.Second)

	token := signUp(t, client, base, dbURL)
	flaky := createJob(t, client, base, token, "flaky:1")
	broken := createJob(t, client, base, token, "fail:permanent")

	deadline := time.Now().Add(20 * time.Second)
	for {
		fs, fa := jobState(t, client, base, token, flaky)
		bs, ba := jobState(t, client, base, token, broken)
		if fs == "completed" && bs == "dead" {
			if fa != 2 || ba != 1 {
				t.Fatalf("attempts: flaky %d, broken %d; want 2 and 1", fa, ba)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after 20s: flaky %s (%d attempts), broken %s (%d); want completed and dead", fs, fa, bs, ba)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
