package events

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/work-frame/goflow/internal/job"
)

// channelListener records which channel Listen subscribed to.
type channelListener struct {
	mu       sync.Mutex
	channels []string
}

func (l *channelListener) Listen(ctx context.Context, channel string, onSubscribed func(), onMessage func([]byte)) error {
	l.mu.Lock()
	l.channels = append(l.channels, channel)
	l.mu.Unlock()
	<-ctx.Done()
	return nil
}

func (l *channelListener) seen() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.channels...)
}

// Production uses one channel for everything; NewPublisherOn and NewHubOn
// move all three uses to another one: the publisher's events, the hub's
// subscription and its canaries. A canary left on the production channel
// would mix two test servers' delivery checks (Task 54).
func TestAnotherChannelMovesEventsSubscriptionAndCanaries(t *testing.T) {
	if Channel != "goflow:events" {
		t.Fatalf("Channel = %q, want goflow:events: production's channel must not move", Channel)
	}
	if NewHub().channel != "goflow:events" || NewPublisher(&recordingSender{}, slog.New(slog.DiscardHandler)).channel != "goflow:events" {
		t.Fatal("NewHub or NewPublisher no longer uses goflow:events")
	}

	const other = "goflow:test:x:events"
	logger := slog.New(slog.DiscardHandler)
	h := NewHubOn(other)

	l := &channelListener{}
	stopListen := Listen(context.Background(), l, h, logger)
	sent := &recordingSender{}
	stopWatch := Watch(context.Background(), sent, h, logger, time.Millisecond, time.Hour)
	p := NewPublisherOn(sent, other, logger)
	stopPublisher := p.Start(context.Background())
	j, err := job.New("moved")
	if err != nil {
		t.Fatal(err)
	}
	j.UserID = "alice"
	p.JobChanged(context.Background(), j)

	deadline := time.Now().Add(5 * time.Second)
	for len(l.seen()) == 0 || len(sent.sent()) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("after 5s: subscribed to %v, %d messages sent", l.seen(), len(sent.sent()))
		}
		time.Sleep(time.Millisecond)
	}
	stopWatch()
	stopPublisher()
	stopListen()

	for _, c := range l.seen() {
		if c != other {
			t.Errorf("the hub subscribed to %q, want %q", c, other)
		}
	}
	sent.mu.Lock()
	defer sent.mu.Unlock()
	for _, c := range sent.channels {
		if c != other {
			t.Errorf("sent on %q, want %q (events and canaries both)", c, other)
		}
	}
}
