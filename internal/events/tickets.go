package events

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// TicketTTL is how long a ticket can be used: long enough to open a stream
// right after asking for it, short enough that a ticket copied from a log
// is already dead.
const TicketTTL = 30 * time.Second

const (
	ticketPrefix    = "gft_"
	ticketKeyPrefix = "goflow:ticket:"
	// ticketLength is the prefix plus 32 random bytes in unpadded base64url.
	ticketLength = len(ticketPrefix) + 43
)

// ErrBadTicket means the ticket is malformed, unknown, expired or already used.
// The four look the same to the caller on purpose.
var ErrBadTicket = errors.New("events: invalid or expired ticket")

// KeyStore is what Tickets needs from Redis. *queue.Queue satisfies it.
type KeyStore interface {
	PutOnce(ctx context.Context, key, value string, ttl time.Duration) error
	TakeOnce(ctx context.Context, key string) (value string, found bool, err error)
}

// Tickets lets a browser open an event stream without putting its session
// token in a URL. A browser can't add an Authorization header to an
// EventSource, so the stream's URL has to carry the credential, and URLs end
// up in proxy logs, browser history and monitoring tools. A ticket in a URL
// is fine where a 24-hour session token isn't: it works once, for 30 seconds,
// and only opens a stream.
//
// Redis stores only the ticket's SHA-256 hash, like sessions in PostgreSQL:
// a copy of Redis's data can't be turned into a working ticket.
type Tickets struct {
	store KeyStore
}

// NewTickets returns Tickets backed by store.
func NewTickets(store KeyStore) Tickets {
	return Tickets{store: store}
}

// Issue returns a new ticket for userID, valid for TicketTTL.
func (t Tickets) Issue(ctx context.Context, userID string) (string, error) {
	b := make([]byte, 32)
	rand.Read(b)
	ticket := ticketPrefix + base64.RawURLEncoding.EncodeToString(b)
	if err := t.store.PutOnce(ctx, ticketKey(ticket), userID, TicketTTL); err != nil {
		return "", err
	}
	return ticket, nil
}

// Redeem returns the user a ticket was issued to and deletes it, in one
// step: a ticket used twice, even at the same moment, works once. It
// returns ErrBadTicket for anything that isn't a live ticket.
func (t Tickets) Redeem(ctx context.Context, ticket string) (userID string, err error) {
	// Checked before Redis: junk never costs a round trip.
	if len(ticket) != ticketLength || !strings.HasPrefix(ticket, ticketPrefix) {
		return "", ErrBadTicket
	}
	userID, found, err := t.store.TakeOnce(ctx, ticketKey(ticket))
	if err != nil {
		return "", err
	}
	if !found {
		return "", ErrBadTicket
	}
	return userID, nil
}

func ticketKey(ticket string) string {
	sum := sha256.Sum256([]byte(ticket))
	return ticketKeyPrefix + hex.EncodeToString(sum[:])
}

// Live is everything the API needs for live updates: tickets and the hub.
type Live struct {
	Tickets Tickets
	Hub     *Hub
}

// IssueTicket returns a new stream ticket for userID.
func (l *Live) IssueTicket(ctx context.Context, userID string) (string, error) {
	return l.Tickets.Issue(ctx, userID)
}

// RedeemTicket returns the user a ticket belongs to, once.
func (l *Live) RedeemTicket(ctx context.Context, ticket string) (string, error) {
	return l.Tickets.Redeem(ctx, ticket)
}

// Subscribed is closed once events can reach the hub (see Hub.Subscribed).
func (l *Live) Subscribed() <-chan struct{} { return l.Hub.Subscribed() }

// Subscribe opens a stream of userID's events (see Hub.Subscribe).
func (l *Live) Subscribe(userID string) (<-chan []byte, func(), bool) {
	return l.Hub.Subscribe(userID)
}
