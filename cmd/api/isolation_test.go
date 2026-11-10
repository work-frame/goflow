package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/work-frame/goflow/internal/queue"
)

// channelRecorder keeps every message published on one Pub/Sub channel.
type channelRecorder struct {
	mu   sync.Mutex
	msgs []string
}

func (r *channelRecorder) add(b []byte) {
	r.mu.Lock()
	r.msgs = append(r.msgs, string(b))
	r.mu.Unlock()
}

// mentioning returns the messages that contain id.
func (r *channelRecorder) mentioning(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, m := range r.msgs {
		if strings.Contains(m, id) {
			n++
		}
	}
	return n
}

// listenTo records a channel until the test ends, from the moment Redis
// confirms the subscription.
func listenTo(t *testing.T, q *queue.Queue, channel string) *channelRecorder {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &channelRecorder{}
	subscribed := make(chan struct{})
	var once sync.Once
	done := make(chan struct{})
	go func() {
		defer close(done)
		q.Listen(ctx, channel, func() { once.Do(func() { close(subscribed) }) }, r.add)
	}()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-subscribed:
	case <-time.After(5 * time.Second):
		t.Fatalf("no subscription to %s within 5s", channel)
	}
	return r
}

// Two test servers, each with its own database, Redis keys and events
// channel, running side by side as two tests will once they run in
// parallel (Task 55). Each runs only its own jobs, and each channel
// carries only its own server's events. Before Task 54 every test server
// used goflow:jobs:* and goflow:events, so one server's workers took the
// other's job IDs, found no such job in their database, and dropped them:
// those jobs never ran (FAILURES.md, case 51).
func TestTwoTestServersShareNoJobsAndNoEvents(t *testing.T) {
	parallelTest(t)
	redisURL := testRedisURL(t)
	newServer := func() (config, string) {
		cfg := testConfig("127.0.0.1:0", newTestDatabaseURL(t))
		cfg.RedisURL = redisURL
		cfg.Workers = 2
		base, _ := startServerWith(t, cfg)
		return cfg, base
	}
	cfgA, baseA := newServer()
	cfgB, baseB := newServer()

	conn, err := queue.Open(context.Background(), redisURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	eventsA := listenTo(t, conn, cfgA.EventsChannel)
	eventsB := listenTo(t, conn, cfgB.EventsChannel)

	client := testClient(t, 5*time.Second)
	tokenA := signUp(t, client, baseA, cfgA.DatabaseURL)
	tokenB := signUp(t, client, baseB, cfgB.DatabaseURL)
	const perServer = 5
	var jobsA, jobsB []string
	for i := range perServer {
		jobsA = append(jobsA, createJob(t, client, baseA, tokenA, fmt.Sprintf("server A job %d", i)))
		jobsB = append(jobsB, createJob(t, client, baseB, tokenB, fmt.Sprintf("server B job %d", i)))
	}

	// Every job finishes on its own server, first try. A job whose ID the
	// other server took would sit in "queued" for good: that server has no
	// such job, so its worker skips and acknowledges the ID.
	deadline := time.Now().Add(10 * time.Second)
	for _, s := range []struct {
		name, base, token string
		ids               []string
	}{{"A", baseA, tokenA, jobsA}, {"B", baseB, tokenB, jobsB}} {
		for _, id := range s.ids {
			for {
				status, attempts := jobState(t, client, s.base, s.token, id)
				if status == "completed" {
					if attempts != 1 {
						t.Errorf("server %s's job %s completed on attempt %d, want 1", s.name, id, attempts)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("server %s's job %s still %s after 10s: the other server's workers took its ID from a shared queue", s.name, id, status)
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
	}

	// Each channel carried its own server's jobs and none of the other's.
	// Checked on its own: the queue's keys and the events channel are
	// separate settings, and Pub/Sub ignores key prefixes.
	time.Sleep(200 * time.Millisecond) // the last "completed" events reach Redis
	for _, c := range []struct {
		name        string
		own, theirs *channelRecorder
		ids         []string
	}{{"A", eventsA, eventsB, jobsA}, {"B", eventsB, eventsA, jobsB}} {
		for _, id := range c.ids {
			if c.own.mentioning(id) == 0 {
				t.Errorf("server %s's job %s never appeared on its own events channel", c.name, id)
			}
			if n := c.theirs.mentioning(id); n > 0 {
				t.Errorf("server %s's job %s appeared %d times on the other server's events channel", c.name, id, n)
			}
		}
	}
}
