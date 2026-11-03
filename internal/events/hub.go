package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"
)

// streamBuffer is how many messages one client may fall behind before the
// hub disconnects it.
const streamBuffer = 64

// Hub hands each event to the open streams of the job's owner, and only to
// them. One Hub per API process; it is safe for concurrent use.
//
// A slow client never holds up the others: delivery doesn't wait. If a
// client's buffer is full, the hub closes its channel, the stream ends, and
// the client reconnects and reloads. Dropping one client is better than
// blocking every event for everyone behind it.
type Hub struct {
	mu      sync.Mutex
	streams map[string]map[*stream]struct{} // by owner's user ID
	closed  bool

	subscribed     chan struct{} // closed once the Redis subscription is first live
	subscribedOnce sync.Once
}

type stream struct {
	ch chan []byte
}

// NewHub returns an empty Hub.
func NewHub() *Hub {
	return &Hub{streams: make(map[string]map[*stream]struct{}), subscribed: make(chan struct{})}
}

// Subscribed is closed once the hub's Redis subscription has been live at
// least once. Until then, events are published into the void, so a stream
// must not tell its client "ready" before this.
func (h *Hub) Subscribed() <-chan struct{} { return h.subscribed }

func (h *Hub) markSubscribed() {
	h.subscribedOnce.Do(func() { close(h.subscribed) })
}

// Subscribe opens a stream of userID's events. msgs closes when the hub
// drops the stream (it fell behind, or the hub closed). Call unsubscribe
// when the client leaves; calling it more than once is fine. ok is false if
// the hub is closed (the server is shutting down).
func (h *Hub) Subscribe(userID string) (msgs <-chan []byte, unsubscribe func(), ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, func() {}, false
	}

	s := &stream{ch: make(chan []byte, streamBuffer)}
	if h.streams[userID] == nil {
		h.streams[userID] = make(map[*stream]struct{})
	}
	h.streams[userID][s] = struct{}{}

	return s.ch, func() { h.remove(userID, s) }, true
}

// remove takes s out of the hub and closes its channel, once.
func (h *Hub) remove(userID string, s *stream) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.removeLocked(userID, s)
}

func (h *Hub) removeLocked(userID string, s *stream) {
	set := h.streams[userID]
	if _, ok := set[s]; !ok {
		return // already removed: closing twice would panic
	}
	delete(set, s)
	if len(set) == 0 {
		delete(h.streams, userID)
	}
	close(s.ch)
}

// Deliver gives msg to every open stream of userID. It never blocks.
func (h *Hub) Deliver(userID string, msg []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.streams[userID] {
		h.send(userID, s, msg)
	}
}

// Broadcast gives msg to every open stream, whoever owns it. Only for
// messages that carry no job data, such as TypeResync.
func (h *Hub) Broadcast(msg []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for userID, set := range h.streams {
		for s := range set {
			h.send(userID, s, msg)
		}
	}
}

// send must be called with h.mu held.
func (h *Hub) send(userID string, s *stream, msg []byte) {
	select {
	case s.ch <- msg:
	default:
		h.removeLocked(userID, s) // fell behind: drop it, it will reconnect
	}
}

// Close ends every stream and refuses new ones. The API calls it when it
// starts shutting down, so open streams don't hold the shutdown up.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for userID, set := range h.streams {
		for s := range set {
			h.removeLocked(userID, s)
		}
	}
}

// Listener is the one Redis call Listen needs. *queue.Queue satisfies it.
type Listener interface {
	Listen(ctx context.Context, channel string, onSubscribed func(), onMessage func([]byte)) error
}

// Listen feeds the hub from Redis until ctx ends or stop is called. Every
// time the Redis subscription is (re)established, every stream is told to
// resync: messages published while it was down are lost.
func Listen(ctx context.Context, l Listener, h *Hub, logger *slog.Logger) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	resync := encoded(Message{Type: TypeResync})

	go func() {
		defer close(done)
		for ctx.Err() == nil {
			err := l.Listen(ctx, Channel,
				func() {
					h.markSubscribed()
					h.Broadcast(resync)
				},
				func(b []byte) {
					var e envelope
					if err := json.Unmarshal(b, &e); err != nil || e.UserID == "" {
						logger.Warn("events: ignoring a malformed message")
						return
					}
					h.Deliver(e.UserID, e.Message)
				})
			if err != nil && ctx.Err() == nil {
				logger.Warn("events: subscription ended; subscribing again", "err", err)
				select {
				case <-ctx.Done():
				case <-time.After(time.Second):
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
