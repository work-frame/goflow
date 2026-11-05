package metrics

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/work-frame/goflow/internal/job"
)

func text(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := scrape(t, m.Handler(token), "Bearer "+token)
	return rec.Body.String()
}

func expect(t *testing.T, body string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if !strings.Contains(body, l) {
			t.Errorf("metrics lack %s", l)
		}
	}
}

func TestJobChangesAndAttempts(t *testing.T) {
	m := New()
	ctx := context.Background()
	for _, s := range []job.Status{job.StatusPending, job.StatusQueued, job.StatusProcessing, job.StatusCompleted, job.StatusPending} {
		m.JobChanged(ctx, &job.Job{Status: s})
	}
	m.JobDeleted(ctx, "u", "j")
	m.ObserveAttempt("completed", 30*time.Millisecond)
	m.ObserveAttempt("retry", 2*time.Second)

	expect(t, text(t, m),
		`goflow_job_status_changes_total{status="pending"} 2`,
		`goflow_job_status_changes_total{status="completed"} 1`,
		`goflow_job_status_changes_total{status="deleted"} 1`,
		`goflow_job_attempt_duration_seconds_count{outcome="completed"} 1`,
		`goflow_job_attempt_duration_seconds_bucket{outcome="retry",le="2"} 1`,
	)
}

func TestQueueSizesAreReadWhenScraped(t *testing.T) {
	m := New()
	calls := 0
	m.WatchQueue(func(ctx context.Context) (int64, int64, int64, error) {
		calls++
		if _, ok := ctx.Deadline(); !ok {
			t.Error("Redis is asked without a deadline: a hung Redis would hang /metrics")
		}
		return 4, 1, 2, nil
	})
	if calls != 0 {
		t.Fatal("Redis asked before anyone read /metrics")
	}
	expect(t, text(t, m),
		`goflow_queue_jobs{list="pending"} 4`,
		`goflow_queue_jobs{list="processing"} 1`,
		`goflow_queue_jobs{list="delayed"} 2`,
	)
}

// When Redis doesn't answer, the queue sizes are missing, not zero: zero
// would claim an empty queue when the truth is "unknown".
func TestQueueSizesAreMissingWhenRedisFails(t *testing.T) {
	m := New()
	m.WatchQueue(func(context.Context) (int64, int64, int64, error) {
		return 0, 0, 0, errors.New("dial tcp: connection refused")
	})
	body := text(t, m)
	if strings.Contains(body, "goflow_queue_jobs{") {
		t.Fatalf("queue sizes reported while Redis was down:\n%s", grep(body, "goflow_queue_jobs"))
	}
}

func TestEventAndLimiterGauges(t *testing.T) {
	m := New()
	degraded := true
	m.WatchEvents(func() int64 { return 7 }, func() int64 { return 2 }, func() int { return 3 })
	m.WatchLimiter(func() bool { return degraded })
	m.RateLimited.WithLabelValues("login_email").Inc()
	expect(t, text(t, m),
		"goflow_events_published_total 7",
		"goflow_events_dropped_total 2",
		"goflow_event_streams 3",
		"goflow_rate_limit_fallback 1",
		`goflow_rate_limited_total{rule="login_email"} 1`,
	)
	degraded = false
	expect(t, text(t, m), "goflow_rate_limit_fallback 0")
}

func grep(body, word string) string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		if strings.Contains(l, word) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
