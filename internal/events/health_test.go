package events

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// Short times so the tests run fast: a pause is due 200 ms after the last
// canary, and checked every 10 ms. 200 ms leaves room for a slow machine
// under the race detector, where a canary can take a while to come back.
const (
	testInterval = 10 * time.Millisecond
	testTimeout  = 200 * time.Millisecond
)

// canaryRedis plays the path a canary takes: PUBLISH, then delivery to
// the subscription (fakeRedis in listen_test.go). Each half can break on
// its own.
type canaryRedis struct {
	mu         sync.Mutex
	refuse     error // PUBLISH fails: Redis is down
	lose       bool  // PUBLISH succeeds, but the subscription never delivers
	deliveries chan []byte
}

func (c *canaryRedis) Publish(ctx context.Context, _ string, b []byte) error {
	c.mu.Lock()
	refuse, lose := c.refuse, c.lose
	c.mu.Unlock()
	if refuse != nil {
		return refuse
	}
	if lose {
		return nil
	}
	select {
	case c.deliveries <- b:
	case <-ctx.Done():
	}
	return nil
}

func (c *canaryRedis) set(refuse error, lose bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refuse, c.lose = refuse, lose
}

// watched starts Listen and Watch on a new hub. The Redis subscription
// isn't confirmed yet: see confirm.
func watched(t *testing.T) (*Hub, *fakeRedis, *canaryRedis) {
	t.Helper()
	f := &fakeRedis{subscribed: make(chan struct{}), payloads: make(chan []byte)}
	c := &canaryRedis{deliveries: f.payloads}
	h := NewHub()
	logger := slog.New(slog.DiscardHandler)
	stopListen := Listen(context.Background(), f, h, logger)
	stopWatch := Watch(context.Background(), c, h, logger, testInterval, testTimeout)
	t.Cleanup(func() {
		stopWatch()
		stopListen()
	})
	return h, f, c
}

// confirm makes Redis confirm the subscription, and takes the resync it
// causes off each stream. Streams must subscribe first: Listen broadcasts
// on its own goroutine, so a stream that subscribes afterwards may or may
// not get the resync.
func confirm(t *testing.T, f *fakeRedis, streams ...<-chan []byte) {
	t.Helper()
	f.subscribed <- struct{}{}
	for _, ch := range streams {
		if got := recv(t, ch); got != `{"type":"resync"}` {
			t.Fatalf("got %s, want the resync of the first subscription", got)
		}
	}
}

// quietFor fails if ch receives anything during d.
func quietFor(t *testing.T, ch <-chan []byte, who string, d time.Duration) {
	t.Helper()
	select {
	case m, ok := <-ch:
		t.Fatalf("%s received %q (open %v), want nothing", who, m, ok)
	case <-time.After(d):
	}
}

// expectPause fails with why unless ch receives "paused" within a second.
func expectPause(t *testing.T, ch <-chan []byte, why string) {
	t.Helper()
	select {
	case m := <-ch:
		if string(m) != `{"type":"paused"}` {
			t.Fatalf("got %s, want paused", m)
		}
	case <-time.After(time.Second):
		t.Fatalf("no pause within 1s: %s", why)
	}
}

// The whole cycle: healthy, paused when canaries stop coming back, resumed
// when they return, and every stream hears both, while staying open.
func TestDeliveryPausesAndResumes(t *testing.T) {
	h, f, c := watched(t)
	alice, _ := subscribe(t, h, "alice")
	bob, _ := subscribe(t, h, "bob")
	confirm(t, f, alice, bob)

	quietFor(t, alice, "alice, while canaries come back", 6*testTimeout)
	if h.Paused() {
		t.Fatal("paused while canaries come back")
	}

	c.set(errors.New("connection refused"), false) // Redis goes away
	for name, ch := range map[string]<-chan []byte{"alice": alice, "bob": bob} {
		if got := recv(t, ch); got != `{"type":"paused"}` {
			t.Fatalf("%s got %s, want paused", name, got)
		}
	}
	if !h.Paused() || h.DeliveryOK() {
		t.Error("Paused() = false after the pause was announced")
	}

	// go-redis subscribes again before canaries flow: that alone proves
	// nothing, so no resync, no resumed.
	f.subscribed <- struct{}{}
	quietFor(t, alice, "alice, after a resubscription during the pause", 3*testInterval)

	c.set(nil, false) // canaries come back
	for name, ch := range map[string]<-chan []byte{"alice": alice, "bob": bob} {
		if got := recv(t, ch); got != `{"type":"resumed"}` {
			t.Fatalf("%s got %s, want resumed", name, got)
		}
	}
	if h.Paused() {
		t.Error("Paused() = true after resuming")
	}
	quietFor(t, alice, "alice, after resuming", 3*testInterval) // one resumed, not one per canary
}

// Case 42: Redis taking the canary (PUBLISH succeeded) is not delivery.
// Only a canary that came back through the subscription counts.
func TestAPublishThatNeverArrivesIsAPause(t *testing.T) {
	h, f, c := watched(t)
	alice, _ := subscribe(t, h, "alice")
	confirm(t, f, alice)
	c.set(nil, true) // PUBLISH works, the subscription is dead
	expectPause(t, alice, "PUBLISH succeeded but no canary came back, and delivery still counts as working")
}

// Every API instance publishes canaries on the same channel. Another
// instance's canary arriving says nothing about this one's own path.
func TestOnlyTheHubsOwnCanariesCount(t *testing.T) {
	h, f, c := watched(t)
	alice, _ := subscribe(t, h, "alice")
	confirm(t, f, alice)
	c.set(nil, true) // this hub's canaries are lost
	other, _ := json.Marshal(envelope{Canary: NewHub().id})
	stop := make(chan struct{})
	defer close(stop)
	go func() { // another instance's canaries keep arriving
		for {
			select {
			case f.payloads <- other:
			case <-stop:
				return
			}
			time.Sleep(testInterval / 2)
		}
	}()
	expectPause(t, alice, "only another hub's canaries came back, and delivery still counts as working")
}

// Before the subscription was ever live, no stream has been told "ready",
// so there is nothing to pause.
func TestNoPauseBeforeTheFirstSubscription(t *testing.T) {
	h, _, c := watched(t) // never confirmed
	alice, _ := subscribe(t, h, "alice")
	c.set(errors.New("connection refused"), false)
	quietFor(t, alice, "alice, before any subscription", 6*testTimeout)
	if h.Paused() {
		t.Error("paused before the subscription was ever live")
	}
}

// A canary never reaches a client.
func TestCanariesReachNoStream(t *testing.T) {
	h, f, _ := watched(t)
	alice, _ := subscribe(t, h, "alice")
	confirm(t, f, alice)
	quietFor(t, alice, "alice", 10*testInterval)
}
