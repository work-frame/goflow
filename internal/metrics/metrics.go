// Package metrics holds GoFlow's Prometheus metrics and serves them at
// /metrics, behind a token.
//
// Metrics are updated where things happen (a request ends, a job changes
// status), never computed when /metrics is read. Reading them is cheap and
// never touches PostgreSQL, so something polling /metrics can't keep the
// database awake. It does keep a free Render instance awake: every request
// counts as activity. That's why there is no hosted collector yet (Phase 10
// decision B); read it with curl when you need it.
package metrics

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// MinTokenLength is the shortest token the API accepts for /metrics.
const MinTokenLength = 32

// Metrics is every metric the API exports, in its own registry (not
// Prometheus's global one, so tests can make as many as they like).
type Metrics struct {
	registry *prometheus.Registry

	// HTTPRequests counts finished requests by route pattern and status
	// code. route is the mux pattern ("GET /api/v1/jobs/{id}"), never the
	// path: one label value per job ID would be one time series per job,
	// without limit. Requests that matched no route share "unmatched".
	HTTPRequests *prometheus.CounterVec
	// HTTPDuration records how long requests took, by route pattern.
	HTTPDuration *prometheus.HistogramVec

	goflowMetrics
}

// New returns the API's metrics, plus Go's own (goroutines, memory, GC)
// and the process's (CPU, open files).
func New() *Metrics {
	m := &Metrics{
		registry:      prometheus.NewRegistry(),
		goflowMetrics: newGoflowMetrics(),
		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "goflow_http_requests_total",
			Help: "Finished HTTP requests, by route pattern and status code.",
		}, []string{"route", "code"}),
		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "goflow_http_request_duration_seconds",
			Help: "How long HTTP requests took, by route pattern. Event streams are left out: they last as long as the browser tab.",
			// From 5 ms to 10 s: a warm request takes milliseconds, a
			// bcrypt login on 0.1 CPU about half a second.
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"route"}),
	}
	m.registry.MustRegister(
		m.HTTPRequests,
		m.HTTPDuration,
		m.JobStatusChanges,
		m.AttemptDuration,
		m.RateLimited,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Handler serves the metrics in Prometheus's text format to requests that
// carry "Authorization: Bearer <token>". The token is compared in constant
// time, through SHA-256 so even its length doesn't leak. Anything else gets
// 401: the numbers show how busy GoFlow is and how it fails, which is
// nobody else's business.
func (m *Metrics) Handler(token string) http.Handler {
	want := sha256.Sum256([]byte(token))
	serve := promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		sum := sha256.Sum256([]byte(got))
		if !ok || subtle.ConstantTimeCompare(sum[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="goflow-metrics"`)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"a metrics token is required"}` + "\n"))
			return
		}
		serve.ServeHTTP(w, r)
	})
}
