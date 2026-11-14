package main

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/work-frame/goflow/internal/database"
	"github.com/work-frame/goflow/internal/loadgen"
	"github.com/work-frame/goflow/internal/queue"
	"github.com/work-frame/goflow/internal/worker"
)

// The load generator against a real server: 20 jobs, of which one is
// accepted but never finishes. The other 19 count; the lost one is
// reported by ID as unfinished, and stays out of the rate and the
// percentiles without vanishing from the result (Task 56; FAILURES.md,
// case 53).
func TestLoadgenReportsTheJobThatNeverFinishes(t *testing.T) {
	parallelTest(t)
	dbURL := testDatabaseURL(t)
	redisURL := testRedisURL(t)
	cfg := testConfig("127.0.0.1:0", dbURL)
	cfg.RedisURL = redisURL // relay only: the pool below runs the jobs
	base, _ := startServerWith(t, cfg)
	client := testClient(t, 5*time.Second)
	token := signUp(t, client, base, dbURL)

	// A pool whose handler finishes every job at once, except the one
	// titled "lost", which it holds until the test ends: accepted, claimed,
	// never completed.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	db, err := database.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	conn, err := queue.Open(ctx, redisURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	poolCtx, stopPool := context.WithCancel(ctx)
	poolDone := make(chan struct{})
	go func() {
		defer close(poolDone)
		p := &worker.Pool{Queue: conn.WithPrefix(cfg.RedisKeyPrefix), Store: database.NewWorkStore(db), Logger: discardLogger(),
			Workers: 2, DrainTimeout: 100 * time.Millisecond, TakeTimeout: testTakeTimeout,
			Handle: func(ctx context.Context, run worker.Run) error {
				if run.Title == "lost" {
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			}}
		if err := p.Run(poolCtx); err != nil {
			t.Errorf("pool: %v", err)
		}
	}()
	t.Cleanup(func() { stopPool(); <-poolDone })

	const lostAt = 7
	rep, err := loadgen.Run(ctx, loadgen.Config{
		API: base, Token: token, Jobs: 20, Concurrency: 5, Wait: 3 * time.Second, PollEvery: 200 * time.Millisecond,
		Title: func(i int) string {
			if i == lostAt {
				return "lost"
			}
			return "sleep:0s"
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if rep.Created != 20 || rep.Completed != 19 || len(rep.Dead) != 0 || len(rep.CreateErrors) != 0 {
		t.Fatalf("report: %d created, %d completed, %d dead, %d create errors; want 20, 19, 0, 0", rep.Created, rep.Completed, len(rep.Dead), len(rep.CreateErrors))
	}
	if len(rep.Unfinished) != 1 {
		t.Fatalf("report lists %d unfinished jobs %v, want the 1 that never finished: a job missing from the result makes the run look complete", len(rep.Unfinished), rep.Unfinished)
	}
	if title := jobTitle(t, client, base, token, rep.Unfinished[0]); title != "lost" {
		t.Fatalf("the unfinished job is %q, want the one titled lost", title)
	}
	if rep.Elapsed <= 0 || rep.PerSecond != float64(rep.Completed)/rep.Elapsed.Seconds() {
		t.Errorf("rate %v over %v, want completed jobs only, divided by the time to the last completion", rep.PerSecond, rep.Elapsed)
	}
	if rep.P50 <= 0 || rep.P99 < rep.P95 || rep.P95 < rep.P50 || rep.P99 > 3*time.Second {
		t.Errorf("p50 %v, p95 %v, p99 %v: want ordered times from the 19 completed jobs, none as long as the lost one's wait", rep.P50, rep.P95, rep.P99)
	}
	if rep.CreatesTook <= 0 || rep.CreateRate != float64(rep.Created)/rep.CreatesTook.Seconds() {
		t.Errorf("create rate %v over %v, want 20 created divided by the time the creates took", rep.CreateRate, rep.CreatesTook)
	}
	if rep.SeenByEvent+rep.SeenByPoll != 19 {
		t.Errorf("seen by events %d + polls %d, want the 19 completed jobs", rep.SeenByEvent, rep.SeenByPoll)
	}
	if slices.Contains(rep.Unfinished, "") {
		t.Error("an empty job ID in the report")
	}
}

// jobTitle reads a job's title through the API.
func jobTitle(t *testing.T, client *http.Client, base, token, id string) string {
	t.Helper()
	status, _, body := requestAs(t, client, http.MethodGet, base+"/api/v1/jobs/"+id, token, "")
	if status != http.StatusOK {
		t.Fatalf("get %s: status %d, body %s", id, status, body)
	}
	var j struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(body, &j); err != nil {
		t.Fatal(err)
	}
	return j.Title
}
