package main

import (
	"regexp"
	"strings"
	"testing"
)

// composeValue returns a "key: value" setting from a compose service block.
func composeValue(block, key string) string {
	m := regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(key) + `:\s*(.+?)\s*(#.*)?$`).FindStringSubmatch(block)
	if m == nil {
		return ""
	}
	return m[1]
}

// The worker service (Task 49) runs the worker binary from the image the
// api service builds, three times, against the API's PostgreSQL and
// Valkey. A second build, another database or one replica would still
// start, so nothing but this test notices.
func TestComposeRunsThreeWorkersFromTheAPIsImage(t *testing.T) {
	parallelTest(t)
	compose := readRepoFile(t, "compose.yaml")
	api, worker := service(t, compose, "api"), service(t, compose, "worker")

	if got, want := composeValue(worker, "image"), composeValue(api, "image"); got == "" || got != want {
		t.Errorf("worker image %q, want the api's %q: one image for both", got, want)
	}
	if strings.Contains(worker, "build:") {
		t.Error("worker has its own build: the api service builds the image")
	}
	if got := composeValue(worker, "pull_policy"); got != "never" {
		t.Errorf("worker pull_policy %q, want never: goflow:dev exists only on this machine", got)
	}
	if got := composeValue(worker, "entrypoint"); got != `["/usr/local/bin/worker"]` {
		t.Errorf("worker entrypoint %q, want the worker binary (the image's default is the API)", got)
	}
	if got := composeValue(worker, "replicas"); got != "3" {
		t.Errorf("worker replicas %q, want 3", got)
	}
	for _, key := range []string{"GOFLOW_DATABASE_URL", "GOFLOW_REDIS_URL"} {
		if got, want := composeValue(worker, key), composeValue(api, key); got == "" || got != want {
			t.Errorf("worker %s %q, want the api's %q: they must share the work", key, got, want)
		}
	}
	if strings.Contains(worker, "ports:") {
		t.Error("worker publishes ports: it serves nothing, and three replicas would clash")
	}
}
