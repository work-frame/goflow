package events

import (
	"testing"
	"time"
)

// recv returns the next message on ch, or fails after a second.
func recv(t *testing.T, ch <-chan []byte) string {
	t.Helper()
	select {
	case m, ok := <-ch:
		if !ok {
			t.Fatal("stream closed, want a message")
		}
		return string(m)
	case <-time.After(time.Second):
		t.Fatal("no message within 1s")
		return ""
	}
}

// nothing fails if ch has a message waiting (a closed channel counts as one).
func nothing(t *testing.T, ch <-chan []byte, who string) {
	t.Helper()
	select {
	case m, ok := <-ch:
		if ok {
			t.Fatalf("%s received %q, want nothing", who, m)
		}
		t.Fatalf("%s's stream was closed, want it open and empty", who)
	default:
	}
}

func subscribe(t *testing.T, h *Hub, userID string) (<-chan []byte, func()) {
	t.Helper()
	ch, unsubscribe, ok := h.Subscribe(userID)
	if !ok {
		t.Fatal("Subscribe on an open hub returned ok = false")
	}
	return ch, unsubscribe
}

// The safeguard this package exists for: an event reaches only the
// streams of the job's owner.
func TestHubDeliversOnlyToTheOwner(t *testing.T) {
	h := NewHub()
	alice1, _ := subscribe(t, h, "alice")
	alice2, _ := subscribe(t, h, "alice") // a second tab
	bob, _ := subscribe(t, h, "bob")

	h.Deliver("alice", []byte("alice's job"))

	for _, ch := range []<-chan []byte{alice1, alice2} {
		if got := recv(t, ch); got != "alice's job" {
			t.Errorf("alice got %q", got)
		}
	}
	nothing(t, bob, "bob")
}

func TestHubDeliverToNobodyIsFine(t *testing.T) {
	NewHub().Deliver("nobody", []byte("x")) // no streams: must not panic or block
}

func TestHubBroadcastReachesEveryone(t *testing.T) {
	h := NewHub()
	alice, _ := subscribe(t, h, "alice")
	bob, _ := subscribe(t, h, "bob")

	h.Broadcast([]byte(`{"type":"resync"}`))

	recv(t, alice)
	recv(t, bob)
}

func TestHubUnsubscribeClosesTheStreamOnce(t *testing.T) {
	h := NewHub()
	ch, unsubscribe := subscribe(t, h, "alice")

	unsubscribe()
	unsubscribe() // a second call must not close the channel again (panic)

	if _, ok := <-ch; ok {
		t.Fatal("stream still open after unsubscribe")
	}
	h.Deliver("alice", []byte("late")) // no streams left: must not panic
}

// A client that stops reading is dropped, and doesn't hold up anyone else.
func TestHubDropsAStreamThatFallsBehind(t *testing.T) {
	h := NewHub()
	slow, _ := subscribe(t, h, "alice")
	fast, _ := subscribe(t, h, "alice")

	start := time.Now()
	for i := 0; i <= streamBuffer; i++ { // one more than the buffer holds
		h.Deliver("alice", []byte("event"))
		recv(t, fast) // the fast client keeps up
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("delivering took %v: the slow client held up the others", d)
	}

	n := 0
	for range slow { // drains what was buffered, then sees the channel closed
		n++
	}
	if n != streamBuffer {
		t.Errorf("slow client got %d messages before being dropped, want %d", n, streamBuffer)
	}
	h.Deliver("alice", []byte("after"))
	if got := recv(t, fast); got != "after" {
		t.Errorf("fast client got %q after the drop", got)
	}
}

func TestHubCloseEndsStreamsAndRefusesNewOnes(t *testing.T) {
	h := NewHub()
	alice, _ := subscribe(t, h, "alice")
	bob, unsubscribeBob := subscribe(t, h, "bob")

	h.Close()

	for name, ch := range map[string]<-chan []byte{"alice": alice, "bob": bob} {
		if _, ok := <-ch; ok {
			t.Errorf("%s's stream still open after Close", name)
		}
	}
	unsubscribeBob() // after Close: must not panic
	if _, _, ok := h.Subscribe("carol"); ok {
		t.Error("Subscribe after Close returned ok = true")
	}
}

func TestHubCountsOpenStreams(t *testing.T) {
	h := NewHub()
	_, un1 := subscribe(t, h, "alice")
	subscribe(t, h, "alice")
	subscribe(t, h, "bob")
	if n := h.Streams(); n != 3 {
		t.Fatalf("Streams() = %d, want 3", n)
	}
	un1()
	if n := h.Streams(); n != 2 {
		t.Errorf("after one left: %d, want 2", n)
	}
}
