package api

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/work-frame/goflow/internal/job"
)

// countingWaker counts Wake calls.
type countingWaker struct{ n atomic.Int64 }

func (w *countingWaker) Wake() { w.n.Add(1) }

func TestCreateJobWakesTheRelay(t *testing.T) {
	w := &countingWaker{}
	router := signedIn(Deps{Jobs: newTestStore(), Relay: w, DB: okPinger, Logger: discardLogger()})

	mustCreate(t, router, "send welcome email")

	if got := w.n.Load(); got != 1 {
		t.Fatalf("Wake called %d times, want 1", got)
	}
}

func TestCreateJobWithoutARelayStillSavesIt(t *testing.T) {
	router := signedIn(Deps{Jobs: newTestStore(), DB: okPinger, Logger: discardLogger()})

	created := mustCreate(t, router, "no queue configured")

	if rec := do(t, router, http.MethodGet, "/api/v1/jobs/"+created.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("get: status %d, body %s", rec.Code, rec.Body)
	}
}

// A job that wasn't saved must not wake the relay.
func TestFailedCreateDoesNotWakeTheRelay(t *testing.T) {
	w := &countingWaker{}
	router := signedIn(Deps{Jobs: errStore{err: errors.New("database is down")}, Relay: w, DB: okPinger, Logger: discardLogger()})

	if rec := do(t, router, http.MethodPost, "/api/v1/jobs", `{"title":"never saved"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := w.n.Load(); got != 0 {
		t.Errorf("Wake called %d times for a job that wasn't saved", got)
	}
}

// A new job starts pending; the relay, not the API, moves it to queued.
func TestCreatedJobStartsPending(t *testing.T) {
	router := signedIn(Deps{Jobs: newTestStore(), Relay: &countingWaker{}, DB: okPinger, Logger: discardLogger()})

	if got := mustCreate(t, router, "fresh").Status; got != job.StatusPending {
		t.Fatalf("status = %s, want pending", got)
	}
}
