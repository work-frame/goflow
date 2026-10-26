package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestIDIsGenerated(t *testing.T) {
	rec := do(t, newRouter(), http.MethodGet, "/healthz", "")

	id := rec.Header().Get("X-Request-ID")
	if len(id) != 16 {
		t.Errorf("X-Request-ID = %q, want a 16-character ID", id)
	}
}

func TestRequestIDIsReusedWhenSafe(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-ID", "client-abc_123")
	rec := httptest.NewRecorder()
	newRouter().ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Request-ID"); got != "client-abc_123" {
		t.Errorf("X-Request-ID = %q, want client-abc_123", got)
	}
}

func TestRequestIDRejectsUnsafeValues(t *testing.T) {
	for _, bad := range []string{"has space", "semi;colon", strings.Repeat("a", 65)} {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Header.Set("X-Request-ID", bad)
		rec := httptest.NewRecorder()
		newRouter().ServeHTTP(rec, req)

		if got := rec.Header().Get("X-Request-ID"); got == bad || len(got) != 16 {
			t.Errorf("incoming %q: got X-Request-ID %q, want a fresh 16-character ID", bad, got)
		}
	}
}

func TestNotFoundIsJSON(t *testing.T) {
	rec := do(t, newRouter(), http.MethodGet, "/nope", "")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if e := decode[errorResponse](t, rec); e.Error != "not found" {
		t.Errorf("error = %q, want not found", e.Error)
	}
}

func TestMethodNotAllowedIsJSON(t *testing.T) {
	rec := do(t, newRouter(), http.MethodPost, "/healthz", "")

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, "GET") {
		t.Errorf("Allow = %q, want it to include GET", allow)
	}
	if e := decode[errorResponse](t, rec); e.Error != "method not allowed" {
		t.Errorf("error = %q, want method not allowed", e.Error)
	}
}

func TestRecoverPanicsReturnsJSON500(t *testing.T) {
	panicking := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	h := recoverPanics(slog.New(slog.DiscardHandler), panicking)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if e := decode[errorResponse](t, rec); e.Error != "internal error" {
		t.Errorf("error = %q, want internal error", e.Error)
	}
}

func TestLogRequestsWritesOneLinePerRequest(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	router := NewRouter(newTestStore(), okPinger, logger)

	req := httptest.NewRequest(http.MethodGet, "/nope?token=secret", nil)
	req.Header.Set("X-Request-ID", "test-id")
	router.ServeHTTP(httptest.NewRecorder(), req)

	line := logs.String()
	for _, want := range []string{
		`"msg":"request"`,
		`"request_id":"test-id"`,
		`"method":"GET"`,
		`"path":"/nope"`,
		`"status":404`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("log line missing %s\nlog: %s", want, line)
		}
	}
	if strings.Contains(line, "secret") {
		t.Errorf("log line leaked the query string: %s", line)
	}
}
