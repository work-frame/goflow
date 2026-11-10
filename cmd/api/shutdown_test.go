package main

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// Stopping a test server after a few requests takes well under a second.
// It used to take about 5.3 s in roughly one run in five: the client had
// dialed a spare connection it never used, and Shutdown waits up to 5 s for
// a connection that hasn't sent its first request (Go issue 22682; see
// testClient).
// testClient closes the client's idle connections before the server stops
// (Task 51). Without that, one of these twenty rounds hit it in every run
// we tried.
func TestStoppingTheServerDoesNotWaitForSpareConnections(t *testing.T) {
	dbURL := testDatabaseURL(t)
	const rounds, limit = 20, 2 * time.Second
	for i := range rounds {
		var started time.Time
		var took time.Duration
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			// Cleanups run last-registered first, so they run in this
			// order: the client's idle connections close, the clock
			// starts, the server stops, the clock stops.
			t.Cleanup(func() { took = time.Since(started) })
			base, _ := startServer(t, dbURL)
			t.Cleanup(func() { started = time.Now() })
			client := testClient(t, 5*time.Second)

			// The session tests' pattern: a response read to the end, then
			// one closed unread. The connection returns to the pool a moment
			// after the body is closed; a request sent in that moment makes
			// the client dial a spare.
			for range 3 {
				if status, _, body := request(t, client, http.MethodGet, base+"/readyz", ""); status != http.StatusOK {
					t.Fatalf("readyz: status %d, body %s", status, body)
				}
				if status := authed(t, client, http.MethodGet, base+"/api/v1/auth/me", "not-a-token"); status != http.StatusUnauthorized {
					t.Fatalf("me with a bad token: status %d, want 401", status)
				}
			}
		})
		if took > limit {
			t.Fatalf("round %d: stopping the server took %v, want under %v: it waited for an unused client connection", i, took.Round(time.Millisecond), limit)
		}
	}
}
