package main

import (
	"io"
	"net/http"
	"regexp"
	"strconv"
	"testing"
	"time"
)

const testMetricsToken = "an-e2e-metrics-token-of-32-characters-or-more"

func scrapeMetrics(t *testing.T, client *http.Client, base string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+testMetricsToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics: status %d", resp.StatusCode)
	}
	return string(body)
}

// value returns the number on the metrics line that starts with series,
// or -1 if there is none.
func value(body, series string) float64 {
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(series) + ` (\S+)$`).FindStringSubmatch(body)
	if m == nil {
		return -1
	}
	v, _ := strconv.ParseFloat(m[1], 64)
	return v
}

// One job's whole life, read back from /metrics on a real server with
// PostgreSQL, Redis and workers: status changes, the retried and the
// completed attempt, events sent, and the queue sizes from Redis.
func TestMetricsEndToEnd(t *testing.T) {
	parallelTest(t)
	dbURL := testDatabaseURL(t)
	redisURL := testRedisURL(t)
	cfg := testConfig("127.0.0.1:0", dbURL)
	cfg.RedisURL = redisURL
	cfg.Workers = 2
	cfg.MetricsToken = testMetricsToken
	base, _ := startServerWith(t, cfg)
	client := testClient(t, 5*time.Second)

	token := signUp(t, client, base, dbURL)
	id := createJob(t, client, base, token, "flaky:1")
	deadline := time.Now().Add(15 * time.Second)
	for {
		if s, _ := jobState(t, client, base, token, id); s == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("flaky:1 not completed within 15s")
		}
		time.Sleep(100 * time.Millisecond)
	}

	body := scrapeMetrics(t, client, base)
	for series, atLeast := range map[string]float64{
		`goflow_job_status_changes_total{status="pending"}`:                1,
		`goflow_job_status_changes_total{status="retrying"}`:               1,
		`goflow_job_status_changes_total{status="completed"}`:              1,
		`goflow_job_attempt_duration_seconds_count{outcome="retry"}`:       1,
		`goflow_job_attempt_duration_seconds_count{outcome="completed"}`:   1,
		`goflow_events_published_total`:                                    1,
		`goflow_http_requests_total{code="201",route="POST /api/v1/jobs"}`: 1,
		`goflow_queue_jobs{list="pending"}`:                                0,
		`goflow_queue_jobs{list="delayed"}`:                                0,
		`goflow_rate_limit_fallback`:                                       0,
	} {
		if v := value(body, series); v < atLeast {
			t.Errorf("%s = %v, want at least %v", series, v, atLeast)
		}
	}
	if v := value(body, "goflow_rate_limit_fallback"); v != 0 {
		t.Errorf("goflow_rate_limit_fallback = %v with Redis up, want 0", v)
	}
}
