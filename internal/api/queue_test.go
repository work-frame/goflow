package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/work-frame/goflow/internal/job"
)

// fakeQueue records enqueued IDs. With err set, every call fails.
type fakeQueue struct {
	mu  sync.Mutex
	ids []string
	err error
}

func (q *fakeQueue) Enqueue(_ context.Context, id string) error {
	if q.err != nil {
		return q.err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.ids = append(q.ids, id)
	return nil
}

func (q *fakeQueue) PingContext(context.Context) error { return q.err }

func TestCreateJobEnqueuesItsID(t *testing.T) {
	q := &fakeQueue{}
	router := signedIn(Deps{Jobs: newTestStore(), Queue: q, DB: okPinger, Logger: discardLogger()})

	created := mustCreate(t, router, "send welcome email")

	if len(q.ids) != 1 || q.ids[0] != created.ID {
		t.Fatalf("queued IDs = %v, want [%s]", q.ids, created.ID)
	}
}

func TestCreateJobWithoutAQueueStillSavesIt(t *testing.T) {
	router := signedIn(Deps{Jobs: newTestStore(), DB: okPinger, Logger: discardLogger()})

	created := mustCreate(t, router, "no queue configured")

	if rec := do(t, router, http.MethodGet, "/api/v1/jobs/"+created.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("get: status %d, body %s", rec.Code, rec.Body)
	}
}

// This test pins down the bug Task 23 fixes. With Redis down, the client is
// told the request failed, yet the job was saved, and it will never be
// queued. A client that retries creates a second job.
func TestCreateJobDualWriteLeavesASavedJobWhenTheQueueFails(t *testing.T) {
	store := newTestStore()
	router := signedIn(Deps{Jobs: store, Queue: &fakeQueue{err: errors.New("redis: connection refused")}, DB: okPinger, Logger: discardLogger()})

	rec := do(t, router, http.MethodPost, "/api/v1/jobs", `{"title":"lost in between"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	list := decode[listJobsResponse](t, do(t, router, http.MethodGet, "/api/v1/jobs", ""))
	if len(list.Jobs) != 1 || list.Jobs[0].Status != job.StatusPending {
		t.Fatalf("jobs after the failed request = %+v, want 1 pending job: the dual write saved it anyway", list.Jobs)
	}
}

func TestNotReadyWhenQueueIsDown(t *testing.T) {
	router := NewRouter(Deps{Jobs: newTestStore(), Queue: &fakeQueue{err: errors.New("redis down")}, DB: okPinger, Logger: discardLogger()})

	if rec := do(t, router, http.MethodGet, "/readyz", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503 when the queue is down", rec.Code)
	}
	if rec := do(t, router, http.MethodGet, "/healthz", ""); rec.Code != http.StatusOK {
		t.Errorf("healthz status = %d, want 200: liveness must not depend on Redis", rec.Code)
	}
}
