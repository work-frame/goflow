package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/work-frame/goflow/internal/metrics"
)

const metricsToken = "a-metrics-token-of-at-least-32-characters"

func metricsRouter(m *metrics.Metrics, logger *slog.Logger) http.Handler {
	return signedIn(Deps{Jobs: newTestStore(), DB: okPinger, Logger: logger, Metrics: m, MetricsToken: metricsToken})
}

// scrapeText reads /metrics through the router, with the token.
func scrapeText(t *testing.T, h http.Handler) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+metricsToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics: status %d, body %s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// Requests are counted by the route pattern, so a thousand job IDs are
// one time series, and made-up paths all land in "unmatched".
func TestRequestsAreCountedByRoutePattern(t *testing.T) {
	m := metrics.New()
	router := metricsRouter(m, discardLogger())

	j := mustCreate(t, router, "count me")
	for range 3 {
		do(t, router, http.MethodGet, "/api/v1/jobs/"+j.ID, "")
	}
	do(t, router, http.MethodGet, "/api/v1/jobs/does-not-exist", "")
	for i := range 50 {
		do(t, router, http.MethodGet, "/random/"+strconv.Itoa(i), "")
	}

	text := scrapeText(t, router)
	for _, want := range []string{
		`goflow_http_requests_total{code="200",route="GET /api/v1/jobs/{id}"} 3`,
		`goflow_http_requests_total{code="404",route="GET /api/v1/jobs/{id}"} 1`,
		`goflow_http_requests_total{code="201",route="POST /api/v1/jobs"} 1`,
		`goflow_http_requests_total{code="404",route="unmatched"} 50`,
		`goflow_http_request_duration_seconds_count{route="GET /api/v1/jobs/{id}"} 4`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
	for _, leak := range []string{j.ID, "/random/", "does-not-exist"} {
		if strings.Contains(text, leak) {
			t.Errorf("metrics contain a path or ID: %q", leak)
		}
	}
}

// A panic is recorded as the 500 the client got.
func TestPanicsCountAs500(t *testing.T) {
	m := metrics.New()
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("boom") })
	mux := http.NewServeMux()
	mux.Handle("GET /boom", h)
	wrapped := recordMetrics(m, recoverPanics(discardLogger(), mux))
	wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/boom", nil))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+metricsToken)
	m.Handler(metricsToken).ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `goflow_http_requests_total{code="500",route="GET /boom"} 1`) {
		t.Fatalf("panic not counted as 500:\n%s", rec.Body)
	}
}

func TestMetricsRouteNeedsAToken(t *testing.T) {
	m := metrics.New()
	if rec := do(t, signedIn(Deps{Jobs: newTestStore(), DB: okPinger, Logger: discardLogger(), Metrics: m}), http.MethodGet, "/metrics", ""); rec.Code != http.StatusNotFound {
		t.Errorf("no token configured: status %d, want 404", rec.Code)
	}
	if rec := do(t, metricsRouter(m, discardLogger()), http.MethodGet, "/metrics", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("token configured, none sent: status %d, want 401", rec.Code)
	}
}

// Render's health check every 5 seconds isn't logged while it passes.
func TestHealthChecksAreLoggedOnlyWhenTheyFail(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	router := NewRouter(Deps{Jobs: newTestStore(), DB: okPinger, Logger: logger})
	do(t, router, http.MethodGet, "/healthz", "")
	if strings.Contains(logs.String(), "path=/healthz") {
		t.Errorf("a passing health check was logged:\n%s", logs.String())
	}
	do(t, router, http.MethodPost, "/healthz", "") // 405
	if !strings.Contains(logs.String(), "path=/healthz") {
		t.Error("a failing health check (405) wasn't logged")
	}
	do(t, router, http.MethodGet, "/readyz", "")
	if !strings.Contains(logs.String(), "path=/readyz") {
		t.Error("/readyz stopped being logged")
	}
}
