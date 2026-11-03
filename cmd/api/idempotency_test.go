package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// Twenty clients send the same create with the same Idempotency-Key at the
// same moment, through real HTTP into real PostgreSQL. Exactly one job may
// exist afterwards, and every client must get its ID back.
func TestIdempotentCreateUnderConcurrency(t *testing.T) {
	dbURL := testDatabaseURL(t)
	base, _ := startServer(t, dbURL)
	client := &http.Client{Timeout: 10 * time.Second}
	token := signUp(t, client, base, dbURL)

	const n = 20
	codes := make([]int, n)
	jobIDs := make([]string, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodPost, base+"/api/v1/jobs", strings.NewReader(`{"title":"charge card 42"}`))
			if err != nil {
				t.Error(err)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Idempotency-Key", "e2e-charge-42")
			<-start
			resp, err := client.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			var j struct {
				ID string `json:"id"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&j); err != nil {
				t.Errorf("request %d: status %d, decode: %v", i, resp.StatusCode, err)
			}
			codes[i], jobIDs[i] = resp.StatusCode, j.ID
		}()
	}
	close(start)
	wg.Wait()

	created, replayed := 0, 0
	for i, code := range codes {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusOK:
			replayed++
		default:
			t.Errorf("request %d: status %d, want 201 or 200", i, code)
		}
		if jobIDs[i] != jobIDs[0] {
			t.Errorf("request %d got job %q, request 0 got %q", i, jobIDs[i], jobIDs[0])
		}
	}
	if created != 1 || replayed != n-1 {
		t.Fatalf("%d created, %d replayed; want 1 and %d", created, replayed, n-1)
	}

	status, _, body := requestAs(t, client, http.MethodGet, base+"/api/v1/jobs", token, "")
	if status != http.StatusOK {
		t.Fatalf("list: status %d", status)
	}
	var list struct {
		Jobs []json.RawMessage `json:"jobs"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Jobs) != 1 {
		t.Fatalf("%d jobs stored, want 1", len(list.Jobs))
	}
}

// A dead job, re-run through the API, goes the whole way again: outbox →
// relay → Redis → worker. "fail:permanent" dies on every run, so it ends
// dead again, but only after a worker has run it a second time.
func TestRetryDeadJobEndToEnd(t *testing.T) {
	dbURL := testDatabaseURL(t)
	redisURL := testRedisURL(t)

	cfg := testConfig("127.0.0.1:0", dbURL)
	cfg.RedisURL = redisURL
	cfg.Workers = 2
	base, _ := startServerWith(t, cfg)
	client := &http.Client{Timeout: 5 * time.Second}
	token := signUp(t, client, base, dbURL)
	id := createJob(t, client, base, token, "fail:permanent")

	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			s, a := jobState(t, client, base, token, id)
			if s == want {
				if a != 1 {
					t.Fatalf("%s with %d attempts, want 1", s, a)
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("after 10s: status %s (%d attempts), want %s", s, a, want)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	waitFor("dead")

	status, _, body := requestAs(t, client, http.MethodPost, base+"/api/v1/jobs/"+id+"/retry", token, "")
	if status != http.StatusOK {
		t.Fatalf("retry: status %d, body %s", status, body)
	}
	var rerun struct {
		Status   string `json:"status"`
		Attempts int    `json:"attempts"`
	}
	if err := json.Unmarshal(body, &rerun); err != nil {
		t.Fatal(err)
	}
	if rerun.Status != "pending" || rerun.Attempts != 0 {
		t.Fatalf("retry returned %s with %d attempts, want pending with 0", rerun.Status, rerun.Attempts)
	}

	// pending with 0 attempts → dead with 1 can only happen if a worker ran it.
	waitFor("dead")
}
