package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/work-frame/goflow/internal/job"
)

func newTestStore() JobStore { return memStore{m: job.NewMemoryStore()} }

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func newRouter() http.Handler {
	return NewRouter(newTestStore(), okPinger, discardLogger())
}

// do sends a request to h and returns the recorded response.
// A non-empty body is sent with Content-Type: application/json.
func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decode parses the response body as JSON into a value of type T.
func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func mustCreate(t *testing.T, h http.Handler, title string) job.Job {
	t.Helper()
	rec := do(t, h, http.MethodPost, "/api/v1/jobs", `{"title":"`+title+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %q: status %d, body %s", title, rec.Code, rec.Body)
	}
	return decode[job.Job](t, rec)
}

func TestHealth(t *testing.T) {
	rec := do(t, newRouter(), http.MethodGet, "/healthz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if body := decode[map[string]string](t, rec); body["status"] != "ok" {
		t.Errorf("status field = %q, want ok", body["status"])
	}
}

func TestHealthRejectsWrongMethod(t *testing.T) {
	rec := do(t, newRouter(), http.MethodPost, "/healthz", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	rec := do(t, newRouter(), http.MethodGet, "/nope", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestCreateJob(t *testing.T) {
	rec := do(t, newRouter(), http.MethodPost, "/api/v1/jobs", `{"title":"send welcome email"}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body %s", rec.Code, http.StatusCreated, rec.Body)
	}
	got := decode[job.Job](t, rec)
	if got.Title != "send welcome email" {
		t.Errorf("Title = %q", got.Title)
	}
	if got.Status != job.StatusPending {
		t.Errorf("Status = %q, want pending", got.Status)
	}
	if loc := rec.Header().Get("Location"); loc != "/api/v1/jobs/"+got.ID {
		t.Errorf("Location = %q, want /api/v1/jobs/%s", loc, got.ID)
	}
}

func TestCreateJobRequestValidation(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		want        int
	}{
		{"valid with charset", "application/json; charset=utf-8", `{"title":"x"}`, http.StatusCreated},
		{"empty body", "application/json", "", http.StatusBadRequest},
		{"malformed JSON", "application/json", `{"title":`, http.StatusBadRequest},
		{"wrong field type", "application/json", `{"title":123}`, http.StatusBadRequest},
		{"unknown field", "application/json", `{"title":"x","priority":5}`, http.StatusBadRequest},
		{"two JSON objects", "application/json", `{"title":"a"}{"title":"b"}`, http.StatusBadRequest},
		{"blank title", "application/json", `{"title":"   "}`, http.StatusBadRequest},
		{"title too long", "application/json", `{"title":"` + strings.Repeat("a", 201) + `"}`, http.StatusBadRequest},
		{"body too large", "application/json", `{"title":"` + strings.Repeat("a", 2<<20) + `"}`, http.StatusRequestEntityTooLarge},
		{"missing content type", "", `{"title":"x"}`, http.StatusUnsupportedMediaType},
		{"wrong content type", "text/plain", `{"title":"x"}`, http.StatusUnsupportedMediaType},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(tc.body))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			rec := httptest.NewRecorder()
			newRouter().ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d, body %s", rec.Code, tc.want, rec.Body)
			}
			if rec.Code >= 400 {
				if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
					t.Errorf("error Content-Type = %q, want application/json", ct)
				}
				if e := decode[errorResponse](t, rec); e.Error == "" {
					t.Error("error message is empty")
				}
			}
		})
	}
}

func TestGetJob(t *testing.T) {
	h := newRouter()
	created := mustCreate(t, h, "send email")

	rec := do(t, h, http.MethodGet, "/api/v1/jobs/"+created.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := decode[job.Job](t, rec); got.ID != created.ID {
		t.Errorf("ID = %q, want %q", got.ID, created.ID)
	}
}

func TestGetMissingJobIs404(t *testing.T) {
	rec := do(t, newRouter(), http.MethodGet, "/api/v1/jobs/does-not-exist", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if e := decode[errorResponse](t, rec); e.Error != "job not found" {
		t.Errorf("error = %q", e.Error)
	}
}

func TestListJobs(t *testing.T) {
	h := newRouter()

	// An empty list must be [] in JSON, never null.
	rec := do(t, h, http.MethodGet, "/api/v1/jobs", "")
	if got := strings.TrimSpace(rec.Body.String()); got != `{"jobs":[]}` {
		t.Errorf("empty list body = %s, want {\"jobs\":[]}", got)
	}

	mustCreate(t, h, "first")
	mustCreate(t, h, "second")

	rec = do(t, h, http.MethodGet, "/api/v1/jobs", "")
	if n := len(decode[listJobsResponse](t, rec).Jobs); n != 2 {
		t.Errorf("got %d jobs, want 2", n)
	}

	rec = do(t, h, http.MethodGet, "/api/v1/jobs?status=completed", "")
	if n := len(decode[listJobsResponse](t, rec).Jobs); n != 0 {
		t.Errorf("status=completed: got %d jobs, want 0", n)
	}

	rec = do(t, h, http.MethodGet, "/api/v1/jobs?status=banana", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=banana: status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestDeleteJob(t *testing.T) {
	h := newRouter()
	created := mustCreate(t, h, "send email")
	path := "/api/v1/jobs/" + created.ID

	rec := do(t, h, http.MethodDelete, path, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("delete: body = %q, want empty", rec.Body.String())
	}

	if rec := do(t, h, http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
		t.Errorf("get after delete: status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if rec := do(t, h, http.MethodDelete, path, ""); rec.Code != http.StatusNotFound {
		t.Errorf("second delete: status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}
