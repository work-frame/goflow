package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/work-frame/goflow/internal/events"
)

// LiveEvents is what the API needs for live job updates. *events.Live
// satisfies it. nil in Deps means no Redis is configured: the endpoints
// answer 503 and everything else works as before.
type LiveEvents interface {
	IssueTicket(ctx context.Context, userID string) (string, error)
	// RedeemTicket returns events.ErrBadTicket for anything but a live ticket.
	RedeemTicket(ctx context.Context, ticket string) (userID string, err error)
	Subscribe(userID string) (msgs <-chan []byte, unsubscribe func(), ok bool)
	// Subscribed is closed once events can arrive at all (the API's Redis
	// subscription has been live once).
	Subscribed() <-chan struct{}
}

// defaultHeartbeat is how often an idle stream sends an SSE comment line.
// Proxies close connections that stay silent too long; a comment keeps
// this one open without the client seeing anything. It is outbound
// traffic, so it doesn't keep a free Render instance awake.
const defaultHeartbeat = 25 * time.Second

// readyMessage is the first message on every stream.
var readyMessage = []byte(`{"type":"` + events.TypeReady + `"}`)

type ticketResponse struct {
	Ticket    string `json:"ticket"`
	ExpiresIn int    `json:"expires_in"` // seconds
}

// createTicket issues a single-use, 30-second ticket for opening the
// caller's event stream. It needs a session like every job route.
func (h *handler) createTicket(w http.ResponseWriter, r *http.Request) {
	if h.live == nil {
		writeError(w, http.StatusServiceUnavailable, "live updates are not available")
		return
	}
	ctx, cancel := dbContext(r)
	defer cancel()

	ticket, err := h.live.IssueTicket(ctx, callerID(r))
	if err != nil {
		h.logger.Warn("can't issue a stream ticket", "request_id", RequestID(r.Context()), "err", err)
		writeError(w, http.StatusServiceUnavailable, "live updates are not available")
		return
	}
	// A credential, like the login token: never cached anywhere.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, ticketResponse{Ticket: ticket, ExpiresIn: int(events.TicketTTL / time.Second)})
}

// streamEvents is GET /api/v1/events?ticket=...: a Server-Sent Events
// stream of the ticket owner's job changes. Each message is one JSON
// object on a "data:" line (see events.Message), and the first is always
// {"type":"ready"}.
//
// The order matters: the stream subscribes to the hub, and waits until the
// API's Redis subscription is live, BEFORE it sends "ready"; the client
// loads its job list only AFTER it sees "ready". Any change committed after
// the list was read then arrives as an event, so nothing falls in between.
// The client keeps whichever version of a job has the later updated_at.
func (h *handler) streamEvents(w http.ResponseWriter, r *http.Request) {
	if h.live == nil {
		writeError(w, http.StatusServiceUnavailable, "live updates are not available")
		return
	}

	ticket := r.URL.Query().Get("ticket")
	if ticket == "" {
		writeError(w, http.StatusUnauthorized, "a ticket is required: POST /api/v1/events/ticket")
		return
	}
	ctx, cancel := dbContext(r)
	userID, err := h.live.RedeemTicket(ctx, ticket)
	cancel()
	if errors.Is(err, events.ErrBadTicket) {
		writeError(w, http.StatusUnauthorized, "invalid or expired ticket")
		return
	}
	if err != nil {
		h.logger.Warn("can't redeem a stream ticket", "request_id", RequestID(r.Context()), "err", err)
		writeError(w, http.StatusServiceUnavailable, "live updates are not available")
		return
	}

	msgs, unsubscribe, ok := h.live.Subscribe(userID)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "server is shutting down")
		return
	}
	defer unsubscribe()

	// The server's read and write timeouts (10 s) are right for normal
	// requests and would cut a stream off after 10 seconds. Lift them for
	// this connection only. ResponseController reaches the real connection
	// through the middleware's Unwrap methods.
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		h.internalError(w, r, fmt.Errorf("stream: lift write deadline: %w", err))
		return
	}
	if err := rc.SetReadDeadline(time.Time{}); err != nil {
		h.internalError(w, r, fmt.Errorf("stream: lift read deadline: %w", err))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no") // tells nginx-style proxies not to hold messages back
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil { // the client sees 200 now, even if "ready" waits
		return
	}

	send := func(data []byte) error {
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return err
		}
		return rc.Flush()
	}
	ping := func() error {
		if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
			return err
		}
		return rc.Flush()
	}

	beat := h.heartbeat
	if beat <= 0 {
		beat = defaultHeartbeat
	}
	ticker := time.NewTicker(beat)
	defer ticker.Stop()

	// Wait for the Redis subscription. Only right after the API starts (or
	// while Redis is unreachable at startup) does this take any time.
	// Messages that arrive meanwhile are resyncs, and "ready" makes the
	// client load everything anyway, so they are dropped.
waiting:
	for {
		select {
		case <-h.live.Subscribed():
			break waiting
		case <-r.Context().Done():
			return
		case _, open := <-msgs:
			if !open {
				return
			}
		case <-ticker.C:
			if err := ping(); err != nil {
				return
			}
		}
	}
	if err := send(readyMessage); err != nil {
		return
	}

	for {
		select {
		case <-r.Context().Done():
			return // the client left
		case m, open := <-msgs:
			if !open {
				return // fell behind, or the server is shutting down: the client reconnects
			}
			if err := send(m); err != nil {
				return
			}
		case <-ticker.C:
			if err := ping(); err != nil {
				return
			}
		}
	}
}
