package api

import (
	"net/http"
	"testing"

	"github.com/work-frame/goflow/internal/job"
)

// setStatus changes a stored job's status, as a worker would.
func setStatus(t *testing.T, m *job.MemoryStore, id string, status job.Status, attempts int) {
	t.Helper()
	j, err := m.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	j.Status, j.Attempts = status, attempts
	if err := m.Delete(id); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(j); err != nil {
		t.Fatal(err)
	}
}

func TestRetryADeadJob(t *testing.T) {
	m := job.NewMemoryStore()
	w := &countingWaker{}
	router := signedIn(Deps{Jobs: memStore{m: m}, Relay: w, DB: okPinger, Logger: discardLogger()})
	created := mustCreate(t, router, "fail")
	setStatus(t, m, created.ID, job.StatusDead, 3)

	rec := do(t, router, http.MethodPost, "/api/v1/jobs/"+created.ID+"/retry", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s; want 200", rec.Code, rec.Body)
	}
	got := decode[job.Job](t, rec)
	if got.ID != created.ID || got.Status != job.StatusPending || got.Attempts != 0 {
		t.Errorf("after retry: %s %s, %d attempts; want the same job, pending, 0", got.ID, got.Status, got.Attempts)
	}
	if n := w.n.Load(); n != 2 { // once for the create, once for the retry
		t.Errorf("relay woken %d times, want 2", n)
	}
}

// Only a job that has stopped for good can be retried. Retrying a running
// or waiting job would run it twice; retrying a completed one would repeat
// finished work.
func TestRetryRefusesJobsThatArentDead(t *testing.T) {
	for _, status := range []job.Status{job.StatusPending, job.StatusQueued, job.StatusProcessing, job.StatusRetrying, job.StatusCompleted} {
		t.Run(string(status), func(t *testing.T) {
			m := job.NewMemoryStore()
			router := signedIn(Deps{Jobs: memStore{m: m}, DB: okPinger, Logger: discardLogger()})
			created := mustCreate(t, router, "x")
			setStatus(t, m, created.ID, status, 1)

			rec := do(t, router, http.MethodPost, "/api/v1/jobs/"+created.ID+"/retry", "")
			if rec.Code != http.StatusConflict {
				t.Fatalf("status %d, body %s; want 409", rec.Code, rec.Body)
			}
		})
	}
}

func TestRetrySomeoneElsesJobIs404(t *testing.T) {
	router, alice, bob := twoUsers(t)
	created := mustCreate(t, asUser(router, alice), "alice's job")

	rec := do(t, asUser(router, bob), http.MethodPost, "/api/v1/jobs/"+created.ID+"/retry", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404: bob must not learn the job exists", rec.Code)
	}
	if rec := do(t, newRouter(), http.MethodPost, "/api/v1/jobs/nope/retry", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing job: status %d, want 404", rec.Code)
	}
}

func TestRetryNeedsASession(t *testing.T) {
	router, _, _ := twoUsers(t)
	if rec := do(t, router, http.MethodPost, "/api/v1/jobs/abc/retry", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
}
