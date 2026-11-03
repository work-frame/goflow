package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// sseStream collects the data lines of one event stream in the background.
type sseStream struct {
	lines  chan string // each "data:" payload; closed when the stream ends
	cancel context.CancelFunc
}

func ticketFor(t *testing.T, client *http.Client, base, token string) string {
	t.Helper()
	status, header, body := requestAs(t, client, http.MethodPost, base+"/api/v1/events/ticket", token, "")
	if status != http.StatusCreated {
		t.Fatalf("ticket: status %d, body %s", status, body)
	}
	if header.Get("Cache-Control") != "no-store" {
		t.Errorf("ticket response Cache-Control = %q, want no-store", header.Get("Cache-Control"))
	}
	var r struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	return r.Ticket
}

func openEvents(t *testing.T, base, ticket string) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/events?ticket="+ticket, nil)
	resp, err := http.DefaultClient.Do(req) // no client timeout: the stream is meant to stay open
	if err != nil {
		cancel()
		t.Fatalf("open events: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		t.Fatalf("open events: status %d", resp.StatusCode)
	}
	s := &sseStream{lines: make(chan string, 100), cancel: cancel}
	go func() {
		defer close(s.lines)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				s.lines <- data
			}
		}
	}()
	t.Cleanup(cancel)
	return s
}

// next returns the next data payload, "EOF" if the stream ended, or fails.
func (s *sseStream) next(t *testing.T, within time.Duration) string {
	t.Helper()
	select {
	case line, ok := <-s.lines:
		if !ok {
			return "EOF"
		}
		return line
	case <-time.After(within):
		t.Fatalf("no event within %v", within)
		return ""
	}
}

type liveEvent struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Job  *struct {
		ID       string `json:"id"`
		Status   string `json:"status"`
		Attempts int    `json:"attempts"`
		Version  int64  `json:"version"`
	} `json:"job"`
}

// The whole path on real PostgreSQL and Redis: a job created through the
// API is announced by the API (pending), the relay (queued) and a worker
// (processing, completed), to its owner's stream only.
//
// Events from different goroutines can arrive out of order, and "queued"
// is often skipped: the relay pushes the ID to Redis before it marks the
// job queued, and a fast worker claims it in between. So the test does
// what a client must do: keep an event only if its version is higher than
// the copy it already has.
func TestLiveEventsEndToEnd(t *testing.T) {
	dbURL := testDatabaseURL(t)
	redisURL := testRedisURL(t)

	cfg := testConfig("127.0.0.1:0", dbURL)
	cfg.RedisURL = redisURL
	cfg.Workers = 2
	base, stop := startServerWith(t, cfg)
	client := &http.Client{Timeout: 5 * time.Second}

	alice := signUp(t, client, base, dbURL)
	bob := signUp(t, client, base, dbURL)
	aliceTicket := ticketFor(t, client, base, alice)
	aliceEvents := openEvents(t, base, aliceTicket)
	bobEvents := openEvents(t, base, ticketFor(t, client, base, bob))
	for name, s := range map[string]*sseStream{"alice": aliceEvents, "bob": bobEvents} {
		if got := s.next(t, 5*time.Second); got != `{"type":"ready"}` {
			t.Fatalf("%s's first event: %s, want ready", name, got)
		}
	}

	// A ticket works once.
	if status, _, _ := request(t, client, http.MethodGet, base+"/api/v1/events?ticket="+aliceTicket, ""); status != http.StatusUnauthorized {
		t.Errorf("reused ticket: status %d, want 401", status)
	}

	id := createJob(t, client, base, alice, "live e2e")

	var statuses []string // as kept by the client's rule
	var latest int64
	for len(statuses) == 0 || statuses[len(statuses)-1] != "completed" {
		raw := aliceEvents.next(t, 10*time.Second)
		if raw == "EOF" {
			t.Fatalf("alice's stream ended after %v", statuses)
		}
		if strings.Contains(raw, "user_id") {
			t.Fatalf("event leaks the owner: %s", raw)
		}
		var e liveEvent
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		if e.Type == "resync" {
			continue
		}
		if e.Type != "changed" || e.Job == nil || e.Job.ID != id {
			t.Fatalf("unexpected event %s", raw)
		}
		if e.Job.Version <= latest {
			continue // not newer than what the client has: a late arrival
		}
		latest = e.Job.Version
		statuses = append(statuses, e.Job.Status)
	}
	got := strings.Join(statuses, " → ")
	if got != "pending → queued → processing → completed" && got != "pending → processing → completed" {
		t.Errorf("alice's view went %s", got)
	}

	// Bob saw nothing of alice's job. His events would have arrived with
	// alice's, so whatever is waiting on his stream by now is everything.
	for quiet := false; !quiet; {
		select {
		case raw := <-bobEvents.lines:
			if strings.Contains(raw, id) {
				t.Fatalf("bob received alice's job: %s", raw)
			}
		case <-time.After(300 * time.Millisecond):
			quiet = true
		}
	}

	// Shutting down ends open streams at once instead of waiting out
	// Shutdown's 10-second limit.
	start := time.Now()
	stop()
	if got := aliceEvents.next(t, 5*time.Second); got != "EOF" {
		t.Errorf("after shutdown: %s, want the stream to end", got)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("shutdown with open streams took %v", d)
	}
}
