package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// Stopping a test server takes well under a second, even when the client
// holds a connection it dialed and never used. Shutdown waits up to 5 s
// for a connection that hasn't sent its first request (Go issue 22682), so
// testClient closes the client's idle connections before the server stops
// (Task 51; FAILURES.md, case 48).
//
// The spare connection is made on purpose, not left to chance: request B
// is sent while A's response is still open, so the client dials a second
// connection, and the test makes that dial slow. A's body is then read and
// closed, B takes A's connection, and the slow dial lands in the pool
// unused. (The first version of this test waited for the race to happen
// by itself; on the development laptop it never did, and the break-it
// passed.)
func TestStoppingTheServerDoesNotWaitForSpareConnections(t *testing.T) {
	dbURL := testDatabaseURL(t)
	const limit = 2 * time.Second
	var started time.Time
	var took time.Duration
	t.Run("server", func(t *testing.T) {
		// Cleanups run last-registered first, so they run in this order:
		// the client's idle connections close, the clock starts, the
		// server stops, the clock stops.
		t.Cleanup(func() { took = time.Since(started) })
		base, _ := startServer(t, dbURL)
		t.Cleanup(func() { started = time.Now() })
		client := testClient(t, 5*time.Second)

		tr := client.Transport.(*http.Transport)
		dial := tr.DialContext
		var dials atomic.Int32
		spareDialed := make(chan struct{})
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if dials.Add(1) == 1 {
				return dial(ctx, network, addr)
			}
			defer close(spareDialed)
			time.Sleep(300 * time.Millisecond) // B gets A's connection first
			return dial(ctx, network, addr)
		}

		a, err := client.Get(base + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		bDone := make(chan error, 1)
		go func() {
			b, err := client.Get(base + "/healthz")
			if err == nil {
				_, err = io.Copy(io.Discard, b.Body)
				b.Body.Close()
			}
			bDone <- err
		}()
		time.Sleep(50 * time.Millisecond) // B is waiting for a connection
		io.Copy(io.Discard, a.Body)
		a.Body.Close()
		if err := <-bDone; err != nil {
			t.Fatal(err)
		}
		select {
		case <-spareDialed:
		case <-time.After(5 * time.Second):
			t.Fatal("the client never dialed a second connection: this test no longer sets up what it checks")
		}
		time.Sleep(100 * time.Millisecond) // the spare reaches the pool, and the server accepts it
	})
	if took > limit {
		t.Fatalf("stopping the server took %v, want under %v: it waited for a connection the client dialed and never used", took.Round(time.Millisecond), limit)
	}
}
