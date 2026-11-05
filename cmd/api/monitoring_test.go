package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The monitoring profile (Task 46) is configuration in three places that
// must agree: compose.yaml, monitoring/prometheus.yml and the Grafana
// dashboard. Nothing builds or runs them in check.sh, so these tests read
// them and fail where they disagree.

// service returns the block of compose.yaml that defines the named
// service, from its "  name:" line to the next service or section.
func service(t *testing.T, compose, name string) string {
	t.Helper()
	start := strings.Index(compose, "\n  "+name+":\n")
	if start < 0 {
		t.Fatalf("compose.yaml has no service %q", name)
	}
	rest := compose[start+1:]
	next := regexp.MustCompile(`\n(  [a-z]+:\n|[a-z]+:)`).FindStringIndex(rest[1:])
	if next == nil {
		return rest
	}
	return rest[:next[0]+1]
}

// Prometheus and Grafana start only when asked for: a plain
// "docker compose up" stays PostgreSQL, Valkey and the API.
func TestMonitoringIsOptIn(t *testing.T) {
	compose := readRepoFile(t, "compose.yaml")
	for _, name := range []string{"prometheus", "grafana"} {
		if !strings.Contains(service(t, compose, name), `profiles: ["monitoring"]`) {
			t.Errorf(`compose.yaml: %s has no profiles: ["monitoring"], so it would start with every docker compose up`, name)
		}
	}
	for _, name := range []string{"postgres", "valkey", "api"} {
		if strings.Contains(service(t, compose, name), "profiles:") {
			t.Errorf("compose.yaml: %s has a profile, so a plain docker compose up would skip it", name)
		}
	}
}

// Prometheus must send the token the API in compose expects. If they
// differ, every scrape gets 401 and the graphs stay empty.
func TestPrometheusUsesTheAPIsMetricsToken(t *testing.T) {
	apiToken := regexp.MustCompile(`(?m)^\s+GOFLOW_METRICS_TOKEN:\s*(\S+)`).FindStringSubmatch(service(t, readRepoFile(t, "compose.yaml"), "api"))
	if apiToken == nil {
		t.Fatal("compose.yaml: the api service has no GOFLOW_METRICS_TOKEN, so it serves no /metrics")
	}
	scrape := regexp.MustCompile(`(?m)^\s+credentials:\s*(\S+)`).FindStringSubmatch(readRepoFile(t, "monitoring/prometheus.yml"))
	if scrape == nil {
		t.Fatal("monitoring/prometheus.yml sends no credentials")
	}
	if scrape[1] != apiToken[1] {
		t.Errorf("Prometheus sends %q, the API expects %q: every scrape would get 401", scrape[1], apiToken[1])
	}
	if len(apiToken[1]) < 32 {
		t.Errorf("the compose token has %d characters; the API refuses fewer than 32", len(apiToken[1]))
	}
}

// Every metric the dashboard asks for exists in internal/metrics. A typo
// in a query isn't an error in Grafana: the panel just says "No data".
func TestDashboardQueriesKnownMetrics(t *testing.T) {
	var dash struct {
		Panels []struct {
			Title   string `json:"title"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal([]byte(readRepoFile(t, "monitoring/grafana/dashboards/goflow.json")), &dash); err != nil {
		t.Fatalf("dashboard JSON: %v", err)
	}
	var source strings.Builder
	files, _ := filepath.Glob("../../internal/metrics/*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		source.Write(b)
	}
	name := regexp.MustCompile(`goflow_[a-z_]+`)
	queries := 0
	for _, p := range dash.Panels {
		for _, target := range p.Targets {
			queries++
			for _, m := range name.FindAllString(target.Expr, -1) {
				base := m
				for _, suffix := range []string{"_bucket", "_sum", "_count"} {
					base = strings.TrimSuffix(base, suffix)
				}
				if !strings.Contains(source.String(), `"`+base+`"`) {
					t.Errorf("panel %q asks for %s, which internal/metrics doesn't define", p.Title, m)
				}
			}
		}
	}
	if queries == 0 {
		t.Fatal("the dashboard has no queries; the test can't be reading it right")
	}
}
