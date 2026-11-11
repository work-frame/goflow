package loadgen

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Only an API on this machine is accepted, and the refusal comes before
// anything is sent.
func TestCheckLocalAcceptsOnlyThisMachine(t *testing.T) {
	for _, api := range []string{"http://localhost:8080", "http://127.0.0.1:8080", "http://127.0.0.2", "http://[::1]:8080", "https://localhost"} {
		if err := CheckLocal(api); err != nil {
			t.Errorf("CheckLocal(%q) = %v, want nil", api, err)
		}
	}
	for _, api := range []string{
		"https://goflow-api-hewh.onrender.com", "http://192.168.1.10:8080", "http://10.0.0.1",
		"http://localhost.example.com", "http://127.0.0.1.nip.io", "ftp://localhost", "localhost:8080", "",
	} {
		if err := CheckLocal(api); err == nil {
			t.Errorf("CheckLocal(%q) = nil, want a refusal", api)
		}
	}
}

func TestRunRefusesARemoteAPIBeforeSendingAnything(t *testing.T) {
	_, err := Run(context.Background(), Config{API: "https://goflow-api-hewh.onrender.com", Token: "x", Jobs: 1, Concurrency: 1, Wait: time.Second})
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("Run against the hosted API = %v, want a refusal", err)
	}
}

func TestPercentileIsNearestRank(t *testing.T) {
	var ds []time.Duration
	for i := 1; i <= 100; i++ {
		ds = append(ds, time.Duration(i)*time.Millisecond)
	}
	for p, want := range map[int]time.Duration{50: 50 * time.Millisecond, 95: 95 * time.Millisecond, 99: 99 * time.Millisecond, 100: 100 * time.Millisecond} {
		if got := percentile(ds, p); got != want {
			t.Errorf("p%d = %v, want %v", p, got, want)
		}
	}
	if got := percentile(nil, 95); got != 0 {
		t.Errorf("p95 of nothing = %v, want 0", got)
	}
}

// Events are a fast path, not something to depend on (they are lost when
// Redis drops, Phase 8). With no event stream at all, the API's own answer
// still finishes every job, and the report says how each was seen.
func TestJobsFinishWithoutAnyEvents(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/jobs":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":"j%d","title":"sleep:0s","status":"pending"}`, n.Add(1))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/jobs/"):
			json.NewEncoder(w).Encode(map[string]string{"id": strings.TrimPrefix(r.URL.Path, "/api/v1/jobs/"), "title": "sleep:0s", "status": "completed"})
		default: // the events ticket: no live updates on this server
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"no redis"}`))
		}
	}))
	defer srv.Close()

	rep, err := Run(context.Background(), Config{API: srv.URL, Token: "t", Jobs: 12, Concurrency: 4, Wait: 5 * time.Second, PollEvery: 10 * time.Millisecond, PollBatch: 5})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Created != 12 || rep.Completed != 12 || len(rep.Unfinished) != 0 || rep.SeenByPoll != 12 || rep.SeenByEvent != 0 {
		t.Fatalf("report %+v: want 12 created, 12 completed, all seen by poll", rep)
	}
	if rep.PerSecond <= 0 || rep.P95 <= 0 {
		t.Errorf("report %+v: want a rate and a p95", rep)
	}
}
