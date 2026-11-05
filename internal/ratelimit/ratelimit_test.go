package ratelimit

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

var rule = Rule{Name: "test", Limit: 3, Window: time.Minute}

// clock is a time source a test can move.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func memoryLimiter(c *clock) *Limiter {
	l := New(nil, slog.New(slog.DiscardHandler))
	l.local = NewMemory(c.now)
	return l
}

func TestAllowsUpToTheLimitThenRefuses(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	l := memoryLimiter(c)
	ctx := context.Background()

	for i := 1; i <= rule.Limit; i++ {
		if d := l.Allow(ctx, rule, "dan@example.com"); !d.Allowed {
			t.Fatalf("attempt %d refused, want allowed up to %d", i, rule.Limit)
		}
	}
	c.t = c.t.Add(20 * time.Second)
	d := l.Allow(ctx, rule, "dan@example.com")
	if d.Allowed || d.RetryAfter != 40*time.Second {
		t.Fatalf("attempt %d = %+v, want refused with 40s left", rule.Limit+1, d)
	}

	// The window ends a minute after the FIRST attempt; refused attempts
	// don't push it back.
	c.t = c.t.Add(40 * time.Second)
	if d := l.Allow(ctx, rule, "dan@example.com"); !d.Allowed {
		t.Fatal("refused after the window ended, want a fresh window")
	}
}

func TestSubjectsAndRulesCountSeparately(t *testing.T) {
	l := memoryLimiter(&clock{t: time.Unix(1000, 0)})
	ctx := context.Background()
	for range rule.Limit {
		l.Allow(ctx, rule, "a")
	}
	if !l.Allow(ctx, rule, "b").Allowed {
		t.Error("subject b refused because of subject a")
	}
	other := Rule{Name: "other", Limit: 1, Window: time.Minute}
	if !l.Allow(ctx, other, "a").Allowed {
		t.Error("rule other refused because of rule test")
	}
}

// failingCounter is a Redis that is down.
type failingCounter struct{ calls int }

func (f *failingCounter) Count(context.Context, string, time.Duration) (int64, time.Duration, error) {
	f.calls++
	return 0, 0, errors.New("dial tcp 10.0.0.5:6379: connection refused")
}

// With Redis down, limits still hold: they're counted in memory, and the
// outage is logged once, not once per attempt.
func TestFallsBackToMemoryWhenRedisIsDown(t *testing.T) {
	var logs bytes.Buffer
	shared := &failingCounter{}
	l := New(shared, slog.New(slog.NewTextHandler(&logs, nil)))
	ctx := context.Background()

	for range rule.Limit {
		if !l.Allow(ctx, rule, "dan").Allowed {
			t.Fatal("refused within the limit")
		}
	}
	if l.Allow(ctx, rule, "dan").Allowed {
		t.Fatal("allowed past the limit with Redis down: the fallback isn't counting")
	}
	if shared.calls != rule.Limit+1 {
		t.Errorf("Redis tried %d times, want every attempt (%d) so recovery is noticed", shared.calls, rule.Limit+1)
	}
	if n := strings.Count(logs.String(), "Redis unreachable"); n != 1 {
		t.Errorf("outage logged %d times, want once", n)
	}
}

// recordingCounter remembers the keys it was given.
type recordingCounter struct{ keys []string }

func (r *recordingCounter) Count(_ context.Context, key string, w time.Duration) (int64, time.Duration, error) {
	r.keys = append(r.keys, key)
	return 1, w, nil
}

// Redis sees which limit a key is for, never whose it is.
func TestKeysHoldNoEmailOrAddress(t *testing.T) {
	r := &recordingCounter{}
	l := New(r, slog.New(slog.DiscardHandler))
	l.Allow(context.Background(), rule, "dan@example.com")
	l.Allow(context.Background(), rule, "203.0.113.9")
	for _, k := range r.keys {
		if strings.Contains(k, "dan") || strings.Contains(k, "example") || strings.Contains(k, "203.0") {
			t.Errorf("key %q contains the subject", k)
		}
		if !strings.HasPrefix(k, "goflow:rl:test:") || len(k) != len("goflow:rl:test:")+32 {
			t.Errorf("key %q: want goflow:rl:test: and 32 hex characters", k)
		}
	}
	if r.keys[0] == r.keys[1] {
		t.Error("two subjects got the same key")
	}
}

// Every new subject adds a counter. Ended windows are swept away, and the
// map never grows past its cap, however many subjects an attacker invents.
func TestMemoryStaysBounded(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	m := NewMemory(c.now)
	ctx := context.Background()
	for i := range maxMemoryKeys + 500 {
		m.Count(ctx, "k"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+time.Duration(i).String(), time.Minute)
	}
	if n := m.Len(); n > maxMemoryKeys {
		t.Fatalf("%d counters, want at most %d", n, maxMemoryKeys)
	}
	c.t = c.t.Add(2 * time.Minute) // every window has ended
	m.Count(ctx, "new", time.Minute)
	if n := m.Len(); n != 1 {
		t.Errorf("after the windows ended: %d counters, want 1 (the old ones swept)", n)
	}
}
