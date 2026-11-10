package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
)

// Delivery health.
//
// An open stream proves only that the browser reaches the API. Whether job
// events reach the API is a separate question, and go-redis can't answer
// it: it reports a subscription when it's made again, never when it
// stops working. And a quiet system looks exactly like a broken one: no
// events either way.
//
// So the hub asks the question itself. Every CanaryInterval, Watch
// publishes a canary carrying the hub's ID to the events channel, the same
// way job events travel. Listen hands the hub its own canary when it comes
// back. If none has come back for CanaryTimeout, delivery is broken:
// every stream gets {"type":"paused"}. When the next one arrives, every
// stream gets {"type":"resumed"} and reloads its list, since the events
// sent in between are gone.
//
// Only a canary that came back counts. A successful PUBLISH proves that
// Redis took the message, not that this hub received it (case 42).
const (
	// CanaryInterval is how often a canary is sent.
	CanaryInterval = 2 * time.Second
	// CanaryTimeout is how long without a canary back means "paused".
	// Checked at every interval, so a pause is reported within
	// CanaryTimeout + CanaryInterval: 10 seconds.
	CanaryTimeout = 8 * time.Second
)

var (
	pausedMessage  = encoded(Message{Type: TypePaused})
	resumedMessage = encoded(Message{Type: TypeResumed})
)

// Paused reports whether delivery is paused: streams were told "paused"
// and haven't been told "resumed" yet. A stream that opens during a pause
// must say so right after "ready".
func (h *Hub) Paused() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.paused
}

// DeliveryOK is the opposite of Paused, for the metrics gauge.
func (h *Hub) DeliveryOK() bool { return !h.Paused() }

// heard records that the hub's own canary came back at now. If delivery
// was paused, every stream is told it resumed. The state changes and the
// message is queued under one lock, so a stream that checks Paused never
// misses the change that follows it.
func (h *Hub) heard(now time.Time) (pausedFor time.Duration, resumed bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastHeard = now
	if !h.paused {
		return 0, false
	}
	h.paused = false
	h.broadcastLocked(resumedMessage)
	return now.Sub(h.pausedAt), true
}

// check pauses delivery if no canary has come back for timeout. Before the
// Redis subscription was ever live, there is nothing to pause: no stream
// has been told "ready" yet.
func (h *Hub) check(now time.Time, timeout time.Duration) (silentFor time.Duration, paused bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	silentFor = now.Sub(h.lastHeard)
	if !h.live || h.paused || silentFor <= timeout {
		return silentFor, false
	}
	h.paused = true
	h.pausedAt = now
	h.broadcastLocked(pausedMessage)
	return silentFor, true
}

// Watch sends a canary every interval and pauses the hub when none has
// come back for timeout, until ctx ends or stop is called. Listen must run
// on the same hub: it's what hands the canaries back. In production,
// interval and timeout are CanaryInterval and CanaryTimeout.
func Watch(ctx context.Context, send Sender, h *Hub, logger *slog.Logger, interval, timeout time.Duration) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	canary, err := json.Marshal(envelope{Canary: h.id})
	if err != nil {
		panic("events: encode canary: " + err.Error())
	}

	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			// The result doesn't matter here: a canary that Redis refused
			// doesn't come back, and that is what check measures. The
			// publisher already logs Redis errors.
			sendCtx, cancelSend := context.WithTimeout(ctx, interval)
			_ = send.Publish(sendCtx, h.channel, canary)
			cancelSend()

			if silentFor, paused := h.check(time.Now(), timeout); paused {
				logger.Warn("events: live delivery paused; streams stay open and reload when it resumes",
					"no_canary_for", silentFor.Round(time.Millisecond))
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
