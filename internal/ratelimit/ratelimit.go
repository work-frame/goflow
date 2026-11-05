// Package ratelimit counts attempts per subject (an email, an IP address,
// an account) in fixed time windows, and says when a limit is reached.
//
// Counters live in Redis, so every API instance shares them and they
// survive restarts. When Redis can't be reached, a counter in this
// process's memory takes over: weaker (it resets when the process restarts
// and isn't shared), but login stays both available and limited.
package ratelimit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"
)

// Rule is one limit: at most Limit attempts per subject in each Window.
type Rule struct {
	// Name goes into the counter's key and the logs, e.g. "login_email".
	Name   string
	Limit  int
	Window time.Duration
}

// Decision is the answer for one attempt.
type Decision struct {
	Allowed bool
	// RetryAfter is how long until the window ends and the count resets.
	// Set when the attempt is refused.
	RetryAfter time.Duration
}

// Counter adds one to a counter that disappears after window, creating it
// if needed, and returns the new count and how long the counter has left.
// The two must happen as one step: a counter that never gets its expiry
// would never reset (FAILURES.md, case 39).
type Counter interface {
	Count(ctx context.Context, key string, window time.Duration) (count int64, ttl time.Duration, err error)
}

// keyPrefix starts every counter's key in Redis.
const keyPrefix = "goflow:rl:"

// Limiter applies rules. It is safe for concurrent use.
type Limiter struct {
	prefix   string  // start of every key
	shared   Counter // Redis; nil when none is configured
	local    *Memory
	logger   *slog.Logger
	mu       sync.Mutex
	degraded bool // the last shared count failed
}

// New returns a Limiter that counts in shared (Redis) and falls back to
// counting in memory when shared fails. A nil shared counts in memory only.
func New(shared Counter, logger *slog.Logger) *Limiter {
	return &Limiter{prefix: keyPrefix, shared: shared, local: NewMemory(time.Now), logger: logger}
}

// WithKeyPrefix returns a copy of l whose keys start with prefix instead
// of "goflow:rl:". Tests use it so their counters, which live 15 minutes
// in a shared Redis, can't carry over into the next test run.
func (l *Limiter) WithKeyPrefix(prefix string) *Limiter {
	return &Limiter{prefix: prefix, shared: l.shared, local: l.local, logger: l.logger}
}

// Allow counts one attempt by subject under rule and says whether it may
// go ahead. The attempt is counted either way: refused attempts keep
// counting, but the window doesn't move, so it still ends on time.
//
// Subjects are hashed before they become keys: Redis never holds an email
// address or an IP address.
func (l *Limiter) Allow(ctx context.Context, rule Rule, subject string) Decision {
	key := l.prefix + Key(rule, subject)[len(keyPrefix):]

	var count int64
	var ttl time.Duration
	var err error
	if l.shared != nil {
		count, ttl, err = l.shared.Count(ctx, key, rule.Window)
		l.noteShared(err)
	}
	if l.shared == nil || err != nil {
		count, ttl, _ = l.local.Count(ctx, key, rule.Window)
	}

	if count <= int64(rule.Limit) {
		return Decision{Allowed: true}
	}
	if ttl <= 0 {
		ttl = rule.Window // the key just expired: a fresh window starts
	}
	return Decision{Allowed: false, RetryAfter: ttl}
}

// noteShared logs when the shared counter starts failing and when it
// recovers, not on every attempt in between.
func (l *Limiter) noteShared(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case err != nil && !l.degraded:
		l.degraded = true
		l.logger.Warn("rate limit: Redis unreachable, counting in memory (weaker: per process, lost on restart)", "err", err)
	case err == nil && l.degraded:
		l.degraded = false
		l.logger.Info("rate limit: counting in Redis again")
	}
}

// Key returns the counter key for rule and subject: the rule's name and a
// SHA-256 of the subject, so the key says which limit it is but not whose.
func Key(rule Rule, subject string) string {
	sum := sha256.Sum256([]byte(subject))
	return keyPrefix + rule.Name + ":" + hex.EncodeToString(sum[:16])
}
