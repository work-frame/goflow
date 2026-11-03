package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/work-frame/goflow/internal/job"
)

// createWithKey posts a job with an Idempotency-Key header.
func createWithKey(t *testing.T, h http.Handler, title, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(`{"title":"`+title+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(idempotencyKeyHeader, key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCreateWithTheSameKeyReturnsTheFirstJob(t *testing.T) {
	w := &countingWaker{}
	router := signedIn(Deps{Jobs: newTestStore(), Relay: w, DB: okPinger, Logger: discardLogger()})

	first := createWithKey(t, router, "send invoice 42", "7f3c1e9a-invoice-42")
	if first.Code != http.StatusCreated {
		t.Fatalf("first create: status %d, body %s", first.Code, first.Body)
	}
	again := createWithKey(t, router, "send invoice 42", "7f3c1e9a-invoice-42")
	if again.Code != http.StatusOK {
		t.Fatalf("repeated create: status %d, body %s; want 200", again.Code, again.Body)
	}
	if got := again.Header().Get(replayedHeader); got != "true" {
		t.Errorf("%s = %q, want true", replayedHeader, got)
	}

	a, b := decode[job.Job](t, first), decode[job.Job](t, again)
	if a.ID != b.ID {
		t.Fatalf("repeated create made a second job: %s and %s", a.ID, b.ID)
	}
	if loc := again.Header().Get("Location"); loc != "/api/v1/jobs/"+a.ID {
		t.Errorf("Location = %q, want the first job", loc)
	}
	if n := len(decode[listJobsResponse](t, do(t, router, http.MethodGet, "/api/v1/jobs", "")).Jobs); n != 1 {
		t.Errorf("%d jobs stored, want 1", n)
	}
	if got := w.n.Load(); got != 1 {
		t.Errorf("relay woken %d times, want 1: a replay queues nothing", got)
	}
}

func TestCreateWithoutAKeyAlwaysMakesANewJob(t *testing.T) {
	router := newRouter()
	a := mustCreate(t, router, "same title")
	b := mustCreate(t, router, "same title")
	if a.ID == b.ID {
		t.Fatal("two creates without a key returned the same job")
	}
}

// Reusing a key for different work is a client bug. Answering with the
// old job would hide it, so it's refused (IETF draft: 422).
func TestCreateWithAReusedKeyAndADifferentTitleIs422(t *testing.T) {
	router := newRouter()
	if rec := createWithKey(t, router, "send invoice 42", "k-1"); rec.Code != http.StatusCreated {
		t.Fatalf("first create: status %d", rec.Code)
	}
	rec := createWithKey(t, router, "send invoice 43", "k-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body %s; want 422", rec.Code, rec.Body)
	}
}

// Titles are trimmed before they're stored, so " x " and "x" are the same request.
func TestCreateReplayComparesTheStoredTitle(t *testing.T) {
	router := newRouter()
	createWithKey(t, router, "send invoice", "k-trim")
	if rec := createWithKey(t, router, "  send invoice ", "k-trim"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s; want 200", rec.Code, rec.Body)
	}
}

func TestIdempotencyKeysAreScopedToTheirOwner(t *testing.T) {
	router, alice, bob := twoUsers(t)

	a := createWithKey(t, asUser(router, alice), "alice's job", "shared-key")
	b := createWithKey(t, asUser(router, bob), "bob's job", "shared-key")
	if a.Code != http.StatusCreated || b.Code != http.StatusCreated {
		t.Fatalf("statuses %d and %d, want 201 for both: keys are per user", a.Code, b.Code)
	}
	if decode[job.Job](t, a).ID == decode[job.Job](t, b).ID {
		t.Fatal("bob got alice's job back")
	}
}

func TestCreateRejectsBadIdempotencyKeys(t *testing.T) {
	tests := map[string]string{
		"empty":       "",
		"space":       "has space",
		"control":     "tab\there",
		"non-ascii":   "clé",
		"256 chars":   strings.Repeat("k", 256),
		"newline end": "abc\n",
	}
	for name, key := range tests {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(`{"title":"x"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(idempotencyKeyHeader, key)
			rec := httptest.NewRecorder()
			newRouter().ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
		})
	}

	t.Run("two headers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(`{"title":"x"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Add(idempotencyKeyHeader, "a")
		req.Header.Add(idempotencyKeyHeader, "b")
		rec := httptest.NewRecorder()
		newRouter().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("255 chars is fine", func(t *testing.T) {
		if rec := createWithKey(t, newRouter(), "x", strings.Repeat("k", 255)); rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, body %s; want 201", rec.Code, rec.Body)
		}
	})
}

// A storage failure on the replay lookup is a 500 like any other, with no details.
func TestCreateReplayLookupFailureIs500(t *testing.T) {
	store := &dupKeyStore{err: errors.New("dial tcp 10.0.0.5:5432: connection refused")}
	router := signedIn(Deps{Jobs: store, DB: okPinger, Logger: discardLogger()})
	rec := createWithKey(t, router, "x", "k")
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Fatalf("status %d, body %s; want a plain 500", rec.Code, rec.Body)
	}
}

// dupKeyStore says every key is taken, then fails to fetch the job.
type dupKeyStore struct {
	errStore
	err error
}

func (s *dupKeyStore) Add(context.Context, *job.Job) error { return job.ErrDuplicateKey }
func (s *dupKeyStore) GetByIdempotencyKey(context.Context, string, string) (*job.Job, error) {
	return nil, s.err
}
