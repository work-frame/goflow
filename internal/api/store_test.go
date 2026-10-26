package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/work-frame/goflow/internal/job"
)

// memStore adapts job.MemoryStore to JobStore so handler tests run without a database.
type memStore struct {
	m *job.MemoryStore
}

func (s memStore) Add(_ context.Context, j *job.Job) error { return s.m.Add(j) }

func (s memStore) Get(_ context.Context, id string) (*job.Job, error) { return s.m.Get(id) }

func (s memStore) Delete(_ context.Context, id string) error { return s.m.Delete(id) }

func (s memStore) List(_ context.Context, status job.Status) ([]*job.Job, error) {
	all := s.m.List()
	out := make([]*job.Job, 0, len(all))
	for _, j := range all {
		if status == "" || j.Status == status {
			out = append(out, j)
		}
	}
	return out, nil
}

// errStore fails every call with the same error.
type errStore struct {
	err error
}

func (s errStore) Add(context.Context, *job.Job) error                  { return s.err }
func (s errStore) Get(context.Context, string) (*job.Job, error)        { return nil, s.err }
func (s errStore) List(context.Context, job.Status) ([]*job.Job, error) { return nil, s.err }
func (s errStore) Delete(context.Context, string) error                 { return s.err }

// ctxSpyStore records what context the handler gave the store.
// Embedding JobStore supplies the methods it doesn't override.
type ctxSpyStore struct {
	JobStore
	cancelRequest  context.CancelFunc
	hadDeadline    bool
	errAfterCancel error
}

func (s *ctxSpyStore) Get(ctx context.Context, id string) (*job.Job, error) {
	_, s.hadDeadline = ctx.Deadline()
	s.cancelRequest() // the client disconnects while the "query" is running
	s.errAfterCancel = ctx.Err()
	return nil, job.ErrNotFound
}

func TestStoreReceivesRequestContextWithDeadline(t *testing.T) {
	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	spy := &ctxSpyStore{JobStore: newTestStore(), cancelRequest: cancel}
	router := NewRouter(spy, okPinger, discardLogger())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/abc", nil).WithContext(reqCtx)
	router.ServeHTTP(httptest.NewRecorder(), req)

	if !spy.hadDeadline {
		t.Error("store context has no deadline: a slow database would hold the request forever")
	}
	if !errors.Is(spy.errAfterCancel, context.Canceled) {
		t.Errorf("after the client disconnected, store ctx.Err() = %v, want context.Canceled", spy.errAfterCancel)
	}
}

func TestStoreErrorsMapToResponses(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"timeout", fmt.Errorf("database: get job abc: %w", context.DeadlineExceeded), http.StatusServiceUnavailable},
		{"client disconnected", fmt.Errorf("database: get job abc: %w", context.Canceled), statusClientClosedRequest},
		{"anything else", errors.New("dial tcp 10.0.0.5:5432: connect: connection refused"), http.StatusInternalServerError},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			router := NewRouter(errStore{err: tc.err}, okPinger, discardLogger())
			rec := do(t, router, http.MethodGet, "/api/v1/jobs/abc", "")

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if strings.Contains(rec.Body.String(), "10.0.0.5") {
				t.Errorf("response leaked internal details: %s", rec.Body)
			}
		})
	}
}
