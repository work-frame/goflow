package events

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// memKeys is a KeyStore in a map. It records the TTL it was given.
type memKeys struct {
	mu    sync.Mutex
	m     map[string]string
	ttl   time.Duration
	calls int
	err   error
}

func newMemKeys() *memKeys { return &memKeys{m: map[string]string{}} }

func (k *memKeys) PutOnce(_ context.Context, key, value string, ttl time.Duration) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.ttl = ttl
	k.m[key] = value
	return nil
}

func (k *memKeys) TakeOnce(_ context.Context, key string) (string, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls++
	if k.err != nil {
		return "", false, k.err
	}
	v, ok := k.m[key]
	delete(k.m, key)
	return v, ok, nil
}

func TestTicketWorksOnce(t *testing.T) {
	keys := newMemKeys()
	tickets := NewTickets(keys)
	ctx := context.Background()

	ticket, err := tickets.Issue(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ticket, "gft_") || len(ticket) != ticketLength {
		t.Errorf("ticket %q: want gft_ and %d characters", ticket, ticketLength)
	}
	if keys.ttl != 30*time.Second {
		t.Errorf("stored for %v, want 30s", keys.ttl)
	}

	user, err := tickets.Redeem(ctx, ticket)
	if err != nil || user != "alice" {
		t.Fatalf("Redeem = %q, %v; want alice", user, err)
	}
	if _, err := tickets.Redeem(ctx, ticket); !errors.Is(err, ErrBadTicket) {
		t.Fatalf("second Redeem = %v, want ErrBadTicket", err)
	}
}

// Redis holds only a hash: a copy of its data can't be used as a ticket.
func TestTicketIsStoredAsAHash(t *testing.T) {
	keys := newMemKeys()
	ticket, _ := NewTickets(keys).Issue(context.Background(), "alice")
	for key := range keys.m {
		if strings.Contains(key, ticket) || strings.Contains(key, ticket[4:]) {
			t.Fatalf("Redis key %q contains the ticket", key)
		}
		if !strings.HasPrefix(key, ticketKeyPrefix) || len(key) != len(ticketKeyPrefix)+64 {
			t.Errorf("key %q: want %s + 64 hex characters", key, ticketKeyPrefix)
		}
	}
}

func TestMalformedTicketsNeverReachRedis(t *testing.T) {
	keys := newMemKeys()
	tickets := NewTickets(keys)
	for _, bad := range []string{"", "gft_", "gfs_" + strings.Repeat("a", 43), "gft_" + strings.Repeat("a", 44), strings.Repeat("x", 47)} {
		if _, err := tickets.Redeem(context.Background(), bad); !errors.Is(err, ErrBadTicket) {
			t.Errorf("Redeem(%q) = %v, want ErrBadTicket", bad, err)
		}
	}
	if keys.calls != 0 {
		t.Errorf("Redis asked %d times for malformed tickets, want 0", keys.calls)
	}
}

// A Redis failure is not "bad ticket": the API answers 503, not 401.
func TestRedeemPassesStoreErrorsThrough(t *testing.T) {
	keys := newMemKeys()
	keys.err = errors.New("redis down")
	_, err := NewTickets(keys).Redeem(context.Background(), "gft_"+strings.Repeat("a", 43))
	if err == nil || errors.Is(err, ErrBadTicket) {
		t.Fatalf("Redeem with Redis down = %v, want the store's error", err)
	}
}
