package main

import (
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"
)

// redisProxy sits between one test server and the shared test Redis, so a
// test can break that server's Redis connections without touching the
// other tests' (CLIENT KILL TYPE pubsub would hit every subscriber).
type redisProxy struct {
	target, addr string

	mu    sync.Mutex
	ln    net.Listener
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

// proxiedRedisURL starts a proxy to the test Redis and returns a Redis URL
// that goes through it.
func proxiedRedisURL(t *testing.T) (string, *redisProxy) {
	t.Helper()
	u, err := url.Parse(testRedisURL(t))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &redisProxy{target: u.Host, addr: ln.Addr().String(), conns: map[net.Conn]struct{}{}}
	p.serve(ln)
	t.Cleanup(p.stop)
	u.Host = p.addr
	return u.String(), p
}

func (p *redisProxy) serve(ln net.Listener) {
	p.mu.Lock()
	p.ln = ln
	p.mu.Unlock()
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			client, err := ln.Accept()
			if err != nil {
				return // stopped
			}
			server, err := net.Dial("tcp", p.target)
			if err != nil {
				client.Close()
				continue
			}
			p.mu.Lock()
			p.conns[client], p.conns[server] = struct{}{}, struct{}{}
			p.mu.Unlock()
			pipe := func(dst, src net.Conn) {
				io.Copy(dst, src)
				dst.Close()
				src.Close()
			}
			go pipe(server, client)
			go pipe(client, server)
		}
	}()
}

// dropConnections cuts every connection, as CLIENT KILL would, but Redis
// stays reachable: go-redis reconnects within a fraction of a second.
func (p *redisProxy) dropConnections() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		c.Close()
	}
	clear(p.conns)
}

// stop makes Redis unreachable: connections cut, new ones refused. Like a
// Key Value restart, as seen from the API.
func (p *redisProxy) stop() {
	p.mu.Lock()
	ln := p.ln
	p.ln = nil
	p.mu.Unlock()
	if ln != nil {
		ln.Close()
		p.wg.Wait()
	}
	p.dropConnections()
}

// start makes Redis reachable again, at the same address.
func (p *redisProxy) start(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", p.addr)
	if err != nil {
		t.Fatalf("proxy: listen again on %s: %v", p.addr, err)
	}
	p.serve(ln)
}

// nextSkipping returns the next stream payload that isn't skip.
func (s *sseStream) nextSkipping(t *testing.T, within time.Duration, skip string) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		got := s.next(t, time.Until(deadline))
		if got != skip {
			return got
		}
	}
}

// stillOpen fails if the stream ends or says anything during d.
func (s *sseStream) stillOpen(t *testing.T, d time.Duration, while string) {
	t.Helper()
	select {
	case line, ok := <-s.lines:
		if !ok {
			t.Fatalf("%s: the stream ended, want it open", while)
		}
		t.Fatalf("%s: got %s, want nothing", while, line)
	case <-time.After(d):
	}
}

func deliveryGauge(t *testing.T, client *http.Client, base string) float64 {
	t.Helper()
	return value(scrapeMetrics(t, client, base), "goflow_event_delivery_ok")
}

// The requirement from Phase 8 (ARCHITECTURE.md, "Delivery health"): when
// job events stop reaching the API, an open stream says so and stays
// open, and says so again when they flow, so the page can reload.
func TestLiveDeliveryPausesWhileRedisIsAway(t *testing.T) {
	dbURL := testDatabaseURL(t)
	redisURL, proxy := proxiedRedisURL(t)
	cfg := testConfig("127.0.0.1:0", dbURL)
	cfg.RedisURL = redisURL
	cfg.MetricsToken = testMetricsToken
	cfg.CanaryInterval, cfg.CanaryTimeout = 100*time.Millisecond, time.Second
	base, _ := startServerWith(t, cfg)
	client := testClient(t, 5*time.Second)

	token := signUp(t, client, base, dbURL)
	s := openEvents(t, base, ticketFor(t, client, base, token))
	if got := s.next(t, 5*time.Second); got != `{"type":"ready"}` {
		t.Fatalf("first event %s, want ready", got)
	}
	s.stillOpen(t, 1500*time.Millisecond, "healthy") // canaries come back: no pause
	if v := deliveryGauge(t, client, base); v != 1 {
		t.Errorf("goflow_event_delivery_ok = %v while healthy, want 1", v)
	}

	proxy.stop()
	cut := time.Now()
	if got := s.nextSkipping(t, 3*time.Second, `{"type":"resync"}`); got != `{"type":"paused"}` {
		t.Fatalf("after Redis went away: %s, want paused", got) // "EOF" would mean the stream ended
	}
	t.Logf("paused %v after Redis went away", time.Since(cut).Round(10*time.Millisecond))
	s.stillOpen(t, time.Second, "paused")
	if v := deliveryGauge(t, client, base); v != 0 {
		t.Errorf("goflow_event_delivery_ok = %v while paused, want 0", v)
	}

	proxy.start(t)
	back := time.Now()
	if got := s.next(t, 5*time.Second); got != `{"type":"resumed"}` {
		t.Fatalf("after Redis came back: %s, want resumed", got)
	}
	t.Logf("resumed %v after Redis came back", time.Since(back).Round(10*time.Millisecond))
	if v := deliveryGauge(t, client, base); v != 1 {
		t.Errorf("goflow_event_delivery_ok = %v after resuming, want 1", v)
	}
}

// A dropped connection that go-redis replaces at once is not a pause:
// streams get the usual resync and nothing else.
func TestShortRedisDropIsOnlyAResync(t *testing.T) {
	dbURL := testDatabaseURL(t)
	redisURL, proxy := proxiedRedisURL(t)
	cfg := testConfig("127.0.0.1:0", dbURL)
	cfg.RedisURL = redisURL
	cfg.CanaryInterval, cfg.CanaryTimeout = 100*time.Millisecond, 2*time.Second
	base, _ := startServerWith(t, cfg)
	client := testClient(t, 5*time.Second)

	token := signUp(t, client, base, dbURL)
	s := openEvents(t, base, ticketFor(t, client, base, token))
	if got := s.next(t, 5*time.Second); got != `{"type":"ready"}` {
		t.Fatalf("first event %s, want ready", got)
	}

	proxy.dropConnections()
	if got := s.next(t, 3*time.Second); got != `{"type":"resync"}` {
		t.Fatalf("after a dropped connection: %s, want resync", got)
	}
	s.stillOpen(t, 2*time.Second, "after the resync") // no paused: the gap was too short
}
