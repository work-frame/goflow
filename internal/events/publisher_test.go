package events

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/work-frame/goflow/internal/job"
)

// recordingSender records what would go to Redis. If block is set, every
// Publish waits for it to close, like a Redis that stopped answering.
type recordingSender struct {
	mu       sync.Mutex
	channels []string
	payloads [][]byte
	block    chan struct{}
	err      error
}

func (s *recordingSender) Publish(ctx context.Context, channel string, payload []byte) error {
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.channels = append(s.channels, channel)
	s.payloads = append(s.payloads, payload)
	return s.err
}

func (s *recordingSender) sent() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.payloads...)
}

func waitForSent(t *testing.T, s *recordingSender, n int) [][]byte {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := s.sent(); len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("published %d messages within 2s, want %d", len(s.sent()), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func testJob(owner, status string) *job.Job {
	j, _ := job.New("send invoice")
	j.UserID = owner
	j.Status = job.Status(status)
	return j
}

func TestPublisherSendsEventsInOrder(t *testing.T) {
	s := &recordingSender{}
	p := NewPublisher(s, slog.New(slog.DiscardHandler))
	stop := p.Start(context.Background())
	defer stop()

	j := testJob("alice", "pending")
	p.JobChanged(context.Background(), j)
	j.Status = job.StatusQueued // later changes to the caller's job don't touch the first event
	p.JobChanged(context.Background(), j)
	p.JobDeleted(context.Background(), "alice", j.ID)

	got := waitForSent(t, s, 3)
	var types, statuses []string
	for _, b := range got {
		var e envelope
		if err := json.Unmarshal(b, &e); err != nil {
			t.Fatal(err)
		}
		if e.UserID != "alice" {
			t.Errorf("envelope user_id = %q, want alice", e.UserID)
		}
		var m Message
		if err := json.Unmarshal(e.Message, &m); err != nil {
			t.Fatal(err)
		}
		types = append(types, m.Type)
		if m.Job != nil {
			statuses = append(statuses, string(m.Job.Status))
		}
	}
	if strings.Join(types, ",") != "changed,changed,deleted" || strings.Join(statuses, ",") != "pending,queued" {
		t.Errorf("published types %v, statuses %v; want changed,changed,deleted and pending,queued", types, statuses)
	}
	for _, c := range s.channels {
		if c != Channel {
			t.Errorf("published to %q, want %q", c, Channel)
		}
	}
}

// The owner's ID routes the event in Redis but never reaches a client.
func TestClientMessagesNeverContainTheOwner(t *testing.T) {
	s := &recordingSender{}
	p := NewPublisher(s, slog.New(slog.DiscardHandler))
	stop := p.Start(context.Background())
	defer stop()

	p.JobChanged(context.Background(), testJob("user-7f3c", "pending"))
	var e envelope
	if err := json.Unmarshal(waitForSent(t, s, 1)[0], &e); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(e.Message), "user-7f3c") || strings.Contains(string(e.Message), "user_id") {
		t.Fatalf("client message contains the owner: %s", e.Message)
	}
}

// A Redis that hangs must not slow down whoever reports a change: the
// buffer fills, then events are dropped and counted.
func TestPublisherNeverBlocksTheCaller(t *testing.T) {
	s := &recordingSender{block: make(chan struct{})}
	p := NewPublisher(s, slog.New(slog.DiscardHandler))
	stop := p.Start(context.Background())
	defer func() {
		close(s.block)
		stop()
	}()

	start := time.Now()
	for range bufferSize + 50 {
		p.JobChanged(context.Background(), testJob("alice", "pending"))
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("%d calls took %v with Redis hanging, want them not to wait", bufferSize+50, d)
	}
	// One event is held by the blocked Publish, bufferSize wait in the buffer.
	if got := p.Dropped(); got < 49 || got > 50 {
		t.Errorf("Dropped() = %d, want 49 or 50", got)
	}
}

func TestPublisherKeepsGoingAfterErrors(t *testing.T) {
	s := &recordingSender{err: errors.New("redis down")}
	p := NewPublisher(s, slog.New(slog.DiscardHandler))
	stop := p.Start(context.Background())
	defer stop()

	p.JobChanged(context.Background(), testJob("alice", "pending"))
	p.JobChanged(context.Background(), testJob("alice", "queued"))
	waitForSent(t, s, 2) // the second was still tried after the first failed
}

func TestPublisherCountsWhatReachedRedis(t *testing.T) {
	s := &recordingSender{}
	p := NewPublisher(s, slog.New(slog.DiscardHandler))
	stop := p.Start(context.Background())
	defer stop()
	p.JobChanged(context.Background(), testJob("alice", "pending"))
	p.JobChanged(context.Background(), testJob("alice", "queued"))
	waitForSent(t, s, 2)
	deadline := time.Now().Add(time.Second)
	for p.Published() != 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if p.Published() != 2 {
		t.Errorf("Published() = %d, want 2", p.Published())
	}
}
