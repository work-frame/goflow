package client

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/work-frame/goflow/internal/api"
	"github.com/work-frame/goflow/internal/job"
)

// memStore lets the real API router run on job.MemoryStore, with no database.
type memStore struct{ m *job.MemoryStore }

func (s memStore) Add(_ context.Context, j *job.Job) error            { return s.m.Add(j) }
func (s memStore) Get(_ context.Context, id string) (*job.Job, error) { return s.m.Get(id) }
func (s memStore) Delete(_ context.Context, id string) error          { return s.m.Delete(id) }
func (s memStore) List(_ context.Context, p job.ListParams) (job.Page, error) {
	return job.Paginate(s.m.List(), p)
}

type alwaysReady struct{}

func (alwaysReady) PingContext(context.Context) error { return nil }

// newTestClient runs the real API router on a local test server and returns a client for it.
// The client and server are tested together, so they can't drift apart.
func newTestClient(t *testing.T) *Client {
	t.Helper()
	router := api.NewRouter(memStore{m: job.NewMemoryStore()}, alwaysReady{}, slog.New(slog.DiscardHandler))
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return New(srv.URL)
}

func TestClientCreateGetDelete(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	created, err := c.Create(ctx, "send welcome email")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Status != job.StatusPending {
		t.Errorf("Status = %q, want pending", created.Status)
	}

	got, err := c.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "send welcome email" || !got.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("Get = %+v, want the job that was created (%+v)", got, created)
	}

	if err := c.Delete(ctx, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := c.Get(ctx, created.ID); !errors.Is(err, job.ErrNotFound) {
		t.Errorf("Get after Delete err = %v, want job.ErrNotFound", err)
	}
	if err := c.Delete(ctx, created.ID); !errors.Is(err, job.ErrNotFound) {
		t.Errorf("second Delete err = %v, want job.ErrNotFound", err)
	}
}

func TestClientReportsValidationErrors(t *testing.T) {
	c := newTestClient(t)

	_, err := c.Create(context.Background(), "   ")

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Status != http.StatusBadRequest || apiErr.Message != "title is required" {
		t.Errorf("got %d %q, want 400 \"title is required\"", apiErr.Status, apiErr.Message)
	}
}

func TestClientFollowsCursors(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	const n = job.MaxPageSize + 1 // one more than a page holds

	for range n {
		if _, err := c.Create(ctx, "paged job"); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	first, next, err := c.List(ctx, "", "")
	if err != nil {
		t.Fatalf("List page 1: %v", err)
	}
	if len(first) != job.MaxPageSize || next == "" {
		t.Fatalf("page 1: %d jobs, next %q; want %d jobs and a cursor", len(first), next, job.MaxPageSize)
	}

	second, next, err := c.List(ctx, "", next)
	if err != nil {
		t.Fatalf("List page 2: %v", err)
	}
	if len(second) != 1 || next != "" {
		t.Errorf("page 2: %d jobs, next %q; want 1 job and no cursor", len(second), next)
	}
	if len(second) == 1 && second[0].ID == first[len(first)-1].ID {
		t.Error("page 2 repeated the last job of page 1")
	}
}

func TestClientReportsUnreachableServer(t *testing.T) {
	c := New("http://127.0.0.1:1") // nothing listens on port 1

	_, err := c.Get(context.Background(), "abc")
	if err == nil {
		t.Fatal("Get succeeded against an unreachable server")
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Errorf("a connection failure was reported as an API response: %v", err)
	}
}
