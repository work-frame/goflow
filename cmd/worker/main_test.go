package main

import (
	"strings"
	"testing"
)

func TestWorkerCount(t *testing.T) {
	for raw, want := range map[string]int{"": 4, "1": 1, "8": 8, "32": 32} {
		got, err := workerCount(raw)
		if err != nil || got != want {
			t.Errorf("workerCount(%q) = %d, %v; want %d", raw, got, err, want)
		}
	}
	for _, raw := range []string{"0", "33", "-1", "four"} {
		if _, err := workerCount(raw); err == nil || !strings.Contains(err.Error(), "GOFLOW_WORKERS") {
			t.Errorf("workerCount(%q): err = %v, want a GOFLOW_WORKERS error", raw, err)
		}
	}
}

func TestCheckSettingsNeverQuotesURLs(t *testing.T) {
	err := checkSettings("postgres://u:secret-db@db.example.com/db", "redis://:secret-redis@cache.example.com:6379")
	if err == nil {
		t.Fatal("remote URLs without TLS were accepted")
	}
	for _, secret := range []string{"secret-db", "secret-redis"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaks %s: %v", secret, err)
		}
	}
}
