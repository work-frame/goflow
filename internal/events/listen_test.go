package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"
)

// fakeRedis plays a Redis subscription: every value sent on subscribed
// makes it report a (re)subscription, every payload is delivered.
type fakeRedis struct {
	subscribed chan struct{}
	payloads   chan []byte
}

func (f *fakeRedis) Listen(ctx context.Context, channel string, onSubscribed func(), onMessage func([]byte)) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-f.subscribed:
			onSubscribed()
		case b := <-f.payloads:
			onMessage(b)
		}
	}
}

func publishAs(t *testing.T, f *fakeRedis, userID string, m Message) {
	t.Helper()
	b, err := json.Marshal(envelope{UserID: userID, Message: encoded(m)})
	if err != nil {
		t.Fatal(err)
	}
	f.payloads <- b
}

func TestListenRoutesMessagesToTheOwner(t *testing.T) {
	f := &fakeRedis{subscribed: make(chan struct{}), payloads: make(chan []byte)}
	h := NewHub()
	alice, _ := subscribe(t, h, "alice")
	bob, _ := subscribe(t, h, "bob")
	stop := Listen(context.Background(), f, h, slog.New(slog.DiscardHandler))
	defer stop()

	publishAs(t, f, "alice", Message{Type: TypeDeleted, ID: "j1"})
	if got := recv(t, alice); got != `{"type":"deleted","id":"j1"}` {
		t.Errorf("alice got %s", got)
	}
	f.payloads <- []byte("not json") // ignored, and the listener keeps going
	publishAs(t, f, "", Message{Type: TypeDeleted, ID: "j2"})
	publishAs(t, f, "alice", Message{Type: TypeDeleted, ID: "j3"})
	if got := recv(t, alice); got != `{"type":"deleted","id":"j3"}` {
		t.Errorf("alice got %s after a malformed message", got)
	}
	nothing(t, bob, "bob")
}

// After every (re)subscription, every stream is told it may have missed events.
func TestListenAsksEveryoneToResyncAfterReconnecting(t *testing.T) {
	f := &fakeRedis{subscribed: make(chan struct{}), payloads: make(chan []byte)}
	h := NewHub()
	alice, _ := subscribe(t, h, "alice")
	bob, _ := subscribe(t, h, "bob")
	stop := Listen(context.Background(), f, h, slog.New(slog.DiscardHandler))
	defer stop()

	f.subscribed <- struct{}{}
	for name, ch := range map[string]<-chan []byte{"alice": alice, "bob": bob} {
		if got := recv(t, ch); got != `{"type":"resync"}` {
			t.Errorf("%s got %s, want a resync", name, got)
		}
	}
}

func TestHubIsSubscribedOnlyAfterRedisSaysSo(t *testing.T) {
	f := &fakeRedis{subscribed: make(chan struct{}), payloads: make(chan []byte)}
	h := NewHub()
	stop := Listen(context.Background(), f, h, slog.New(slog.DiscardHandler))
	defer stop()

	select {
	case <-h.Subscribed():
		t.Fatal("Subscribed closed before Redis confirmed the subscription")
	default:
	}
	f.subscribed <- struct{}{}
	f.subscribed <- struct{}{} // a reconnect: closing twice must not panic
	select {
	case <-h.Subscribed():
	case <-time.After(time.Second):
		t.Fatal("Subscribed not closed after Redis confirmed the subscription")
	}
}
