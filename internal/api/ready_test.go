package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// fakePinger lets tests choose whether the "database" is up, with no real database.
type fakePinger struct{ err error }

func (p fakePinger) PingContext(ctx context.Context) error { return p.err }

// okPinger is a database that is always reachable.
var okPinger = fakePinger{}

func TestReadyWhenDatabaseIsUp(t *testing.T) {
	rec := do(t, newRouter(), http.MethodGet, "/readyz", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if body := decode[map[string]string](t, rec); body["status"] != "ready" {
		t.Errorf("status field = %q, want ready", body["status"])
	}
}

func TestNotReadyWhenDatabaseIsDown(t *testing.T) {
	down := fakePinger{err: errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")}
	router := NewRouter(Deps{Jobs: newTestStore(), DB: down, Logger: discardLogger()})

	rec := do(t, router, http.MethodGet, "/readyz", "")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if body := decode[map[string]string](t, rec); body["status"] != "not ready" {
		t.Errorf("status field = %q, want not ready", body["status"])
	}
	// The real error stays in the logs. Clients must not learn addresses or ports.
	if strings.Contains(rec.Body.String(), "5432") {
		t.Errorf("response leaked internal details: %s", rec.Body)
	}
}

func TestHealthStaysUpWhenDatabaseIsDown(t *testing.T) {
	down := fakePinger{err: errors.New("database is down")}
	router := NewRouter(Deps{Jobs: newTestStore(), DB: down, Logger: discardLogger()})

	if rec := do(t, router, http.MethodGet, "/healthz", ""); rec.Code != http.StatusOK {
		t.Errorf("healthz status = %d, want %d: liveness must not depend on the database", rec.Code, http.StatusOK)
	}
}
