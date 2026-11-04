package api

import (
	"bytes"
	"encoding/json"
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
	router := NewRouter(Deps{Jobs: newTestStore(), DB: okPinger, Logger: logger})

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

// ipSourcesLine runs one request through a router with LogIPHeaders set
// as given and returns the "client ip sources" log line, or nil.
func ipSourcesLine(t *testing.T, on bool, headers map[string][]string) map[string]any {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	router := NewRouter(Deps{Jobs: newTestStore(), DB: okPinger, Logger: logger, LogIPHeaders: on})

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	req.RemoteAddr = "10.0.0.7:41234"
	for name, values := range headers {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	router.ServeHTTP(httptest.NewRecorder(), req)

	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if m["msg"] == "client ip sources" {
			return m
		}
	}
	return nil
}

func TestIPSourcesAreLoggedWhenOn(t *testing.T) {
	m := ipSourcesLine(t, true, map[string][]string{
		"X-Forwarded-For":  {"1.2.3.4, 203.0.113.9", "198.51.100.1"}, // two header lines
		"Cf-Connecting-Ip": {"203.0.113.9"},
	})
	if m == nil {
		t.Fatal("no client ip sources line")
	}
	want := map[string]string{
		"remote_addr":      "10.0.0.7:41234",
		"X-Forwarded-For":  "1.2.3.4, 203.0.113.9 | 198.51.100.1",
		"CF-Connecting-IP": "203.0.113.9",
		"path":             "/readyz",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %q", k, m[k], v)
		}
	}
	if _, ok := m["True-Client-IP"]; ok {
		t.Error("True-Client-IP logged although the request had none")
	}
	if id, _ := m["request_id"].(string); len(id) != 16 {
		t.Errorf("request_id = %v, want the request's ID", m["request_id"])
	}
}

func TestIPSourcesAreNotLoggedByDefault(t *testing.T) {
	if m := ipSourcesLine(t, false, map[string][]string{"X-Forwarded-For": {"1.2.3.4"}}); m != nil {
		t.Fatalf("logged %v with LogIPHeaders off", m)
	}
}

// A client controls these headers and can send up to 1 MB of them. Each
// logged value is capped, so nobody can fill the logs through this line.
func TestIPSourcesAreCapped(t *testing.T) {
	m := ipSourcesLine(t, true, map[string][]string{"X-Forwarded-For": {strings.Repeat("9", 100_000)}})
	got, _ := m["X-Forwarded-For"].(string)
	if len(got) > maxLoggedHeader+len("...(cut)") || !strings.HasSuffix(got, "...(cut)") {
		t.Fatalf("logged %d characters, want at most %d and a ...(cut) marker", len(got), maxLoggedHeader+len("...(cut)"))
	}
}
