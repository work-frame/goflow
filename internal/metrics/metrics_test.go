package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const token = "a-metrics-token-of-at-least-32-characters"

func scrape(t *testing.T, h http.Handler, auth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHandlerNeedsTheToken(t *testing.T) {
	h := New().Handler(token)
	for name, auth := range map[string]string{
		"no header":      "",
		"wrong token":    "Bearer not-the-token",
		"token prefix":   "Bearer " + token[:10],
		"not bearer":     "Basic " + token,
		"token + extra":  "Bearer " + token + "x",
		"bare token":     token,
		"lowercase type": "bearer x",
	} {
		rec := scrape(t, h, auth)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "goflow_") {
			t.Errorf("%s: metrics leaked without the token", name)
		}
	}
}

func TestHandlerServesPrometheusText(t *testing.T) {
	m := New()
	m.HTTPRequests.WithLabelValues("GET /healthz", "200").Inc()
	rec := scrape(t, m.Handler(token), "Bearer "+token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`goflow_http_requests_total{code="200",route="GET /healthz"} 1`,
		"go_goroutines",
		"process_cpu_seconds_total",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}

// Each New has its own registry: two of them (two test servers) don't
// panic with "duplicate metrics collector registration".
func TestNewCanBeCalledTwice(t *testing.T) {
	New()
	New()
}
