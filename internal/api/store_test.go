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

// memStore adapts job.MemoryStore to JobStore so handler tests run without a
// database. Like database.JobStore, it treats another user's job as missing.
type memStore struct {
	m *job.MemoryStore
}

func (s memStore) Add(_ context.Context, j *job.Job) error { return s.m.Add(j) }

func (s memStore) Get(_ context.Context, userID, id string) (*job.Job, error) {
	j, err := s.m.Get(id)
	if err == nil && j.UserID != userID {
		return nil, job.ErrNotFound
	}
	return j, err
}

func (s memStore) Delete(ctx context.Context, userID, id string) error {
	if _, err := s.Get(ctx, userID, id); err != nil {
		return err
	}
	return s.m.Delete(id)
}

func (s memStore) List(_ context.Context, userID string, p job.ListParams) (job.Page, error) {
	var mine []*job.Job
	for _, j := range s.m.List() {
		if j.UserID == userID {
			mine = append(mine, j)
		}
	}
	return job.Paginate(mine, p)
}

// errStore fails every call with the same error.
type errStore struct {
	err error
}

func (s errStore) Add(context.Context, *job.Job) error                   { return s.err }
func (s errStore) Get(context.Context, string, string) (*job.Job, error) { return nil, s.err }
func (s errStore) List(context.Context, string, job.ListParams) (job.Page, error) {
	return job.Page{}, s.err
}
func (s errStore) Delete(context.Context, string, string) error { return s.err }

// ctxSpyStore records what context the handler gave the store.
// Embedding JobStore supplies the methods it doesn't override.
type ctxSpyStore struct {
	JobStore
	cancelRequest  context.CancelFunc
	hadDeadline    bool
	errAfterCancel error
}

func (s *ctxSpyStore) Get(ctx context.Context, userID, id string) (*job.Job, error) {
	_, s.hadDeadline = ctx.Deadline()
	s.cancelRequest() // the client disconnects while the "query" is running
	s.errAfterCancel = ctx.Err()
	return nil, job.ErrNotFound
}

func TestStoreReceivesRequestContextWithDeadline(t *testing.T) {
	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	spy := &ctxSpyStore{JobStore: newTestStore(), cancelRequest: cancel}
	router := signedIn(Deps{Jobs: spy, DB: okPinger, Logger: discardLogger()})

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
			router := signedIn(Deps{Jobs: errStore{err: tc.err}, DB: okPinger, Logger: discardLogger()})
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
