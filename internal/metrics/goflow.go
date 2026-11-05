package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/work-frame/goflow/internal/job"
)

// goflowMetrics are the job-processing metrics. Every label comes from a
// small set the code decides (a job status, an outcome, a rule name, a
// list name), never from a request (FAILURES.md, case 40).
type goflowMetrics struct {
	// JobStatusChanges counts committed status changes by the status a job
	// moved to, plus "deleted". "pending" counts new jobs and re-runs.
	JobStatusChanges *prometheus.CounterVec
	// AttemptDuration records how long each attempt's handler ran, by how
	// the attempt ended: "completed", "retry" or "dead".
	AttemptDuration *prometheus.HistogramVec
	// RateLimited counts refused requests by rule ("login_email", ...).
	RateLimited *prometheus.CounterVec
}

func newGoflowMetrics() goflowMetrics {
	return goflowMetrics{
		JobStatusChanges: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "goflow_job_status_changes_total",
			Help: "Committed job status changes, by new status. pending includes re-runs; deleted counts deletions.",
		}, []string{"status"}),
		AttemptDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "goflow_job_attempt_duration_seconds",
			Help: "How long each job attempt's handler ran, by outcome: completed, retry or dead.",
			// Up to the 15 s attempt limit, with room above it for a
			// handler that ignored its deadline.
			Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 2, 5, 10, 15, 30, 60},
		}, []string{"outcome"}),
		RateLimited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "goflow_rate_limited_total",
			Help: "Requests refused with 429, by rate-limit rule.",
		}, []string{"rule"}),
	}
}

// JobChanged counts a committed status change. With JobDeleted, it makes
// Metrics a database.Notifier: the stores report every change in one place,
// so none can be missed by a handler.
func (m *Metrics) JobChanged(_ context.Context, j *job.Job) {
	m.JobStatusChanges.WithLabelValues(string(j.Status)).Inc()
}

// JobDeleted counts a deletion.
func (m *Metrics) JobDeleted(context.Context, string, string) {
	m.JobStatusChanges.WithLabelValues("deleted").Inc()
}

// ObserveAttempt records one attempt's outcome and handler time. It fits
// worker.Pool.Observe.
func (m *Metrics) ObserveAttempt(outcome string, took time.Duration) {
	m.AttemptDuration.WithLabelValues(outcome).Observe(took.Seconds())
}

// WatchQueue reports the queue's sizes each time /metrics is read, from
// one Redis round trip with a 1-second limit. It never asks PostgreSQL.
func (m *Metrics) WatchQueue(sizes func(ctx context.Context) (pending, processing, delayed int64, err error)) {
	m.registry.MustRegister(&queueCollector{sizes: sizes, desc: prometheus.NewDesc(
		"goflow_queue_jobs",
		"Job IDs in Redis right now: pending (waiting for a worker), processing (taken, not finished), delayed (retries not due yet). Missing when Redis didn't answer.",
		[]string{"list"}, nil,
	)})
}

// WatchEvents reports live-update counts when /metrics is read.
func (m *Metrics) WatchEvents(published, dropped func() int64, streams func() int) {
	m.registry.MustRegister(
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "goflow_events_published_total",
			Help: "Job events that reached Redis.",
		}, func() float64 { return float64(published()) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "goflow_events_dropped_total",
			Help: "Job events dropped because the publisher's buffer was full (Redis slow or down).",
		}, func() float64 { return float64(dropped()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "goflow_event_streams",
			Help: "Event streams open on this instance.",
		}, func() float64 { return float64(streams()) }),
	)
}

// WatchLimiter reports whether rate limits are being counted in memory
// because Redis is unreachable (1) or in Redis (0).
func (m *Metrics) WatchLimiter(degraded func() bool) {
	m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "goflow_rate_limit_fallback",
		Help: "1 while rate limits are counted in memory because Redis is unreachable, else 0.",
	}, func() float64 {
		if degraded() {
			return 1
		}
		return 0
	}))
}

// WatchDelivery reports whether job events reach this API (1) or
// delivery is paused because its canaries stopped coming back (0). See
// events/health.go.
func (m *Metrics) WatchDelivery(ok func() bool) {
	m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "goflow_event_delivery_ok",
		Help: "1 while job events reach this API, 0 while live delivery is paused (no canary back for 8 s).",
	}, func() float64 {
		if ok() {
			return 1
		}
		return 0
	}))
}

// queueCollector asks Redis for the queue's sizes at the moment /metrics
// is read.
type queueCollector struct {
	sizes func(ctx context.Context) (pending, processing, delayed int64, err error)
	desc  *prometheus.Desc
}

func (c *queueCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

// Collect sends nothing when Redis doesn't answer. Reporting 0 would say
// "the queue is empty" when the truth is "nobody knows": a graph would show
// a calm queue during an outage (FAILURES.md, case 41).
func (c *queueCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	pending, processing, delayed, err := c.sizes(ctx)
	if err != nil {
		return
	}
	for list, n := range map[string]int64{"pending": pending, "processing": processing, "delayed": delayed} {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(n), list)
	}
}
