package ratelimit

import (
	"context"
	"sync"
	"time"
)

// maxMemoryKeys caps the in-memory counters. Every new email or IP address
// adds one, so without a cap an attacker could grow the map until the
// process runs out of memory.
const maxMemoryKeys = 100_000

// Memory is a Counter in this process's memory: the fallback when Redis
// is unreachable, and the only counter when no Redis is configured.
type Memory struct {
	mu        sync.Mutex
	now       func() time.Time
	keys      map[string]*window
	lastSweep time.Time
}

type window struct {
	count int64
	ends  time.Time
}

// NewMemory returns an empty Memory that reads the time from now.
func NewMemory(now func() time.Time) *Memory {
	return &Memory{now: now, keys: make(map[string]*window)}
}

// Count adds one to key's counter in the current window. It never fails.
func (m *Memory) Count(_ context.Context, key string, length time.Duration) (int64, time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()

	w, ok := m.keys[key]
	if !ok || !now.Before(w.ends) {
		// A sweep walks every counter, so at most one per second: a full
		// map of live windows mustn't make every new attempt that slow.
		if !ok && len(m.keys) >= maxMemoryKeys && now.Sub(m.lastSweep) >= time.Second {
			m.sweep(now)
			m.lastSweep = now
		}
		if len(m.keys) >= maxMemoryKeys {
			// Still full of live windows: someone is creating counters
			// as fast as they can. Count nothing new rather than grow;
			// the attempt goes ahead, still limited per IP if that
			// counter already exists.
			return 1, length, nil
		}
		w = &window{ends: now.Add(length)}
		m.keys[key] = w
	}
	w.count++
	return w.count, w.ends.Sub(now), nil
}

// sweep drops every window that has ended. The caller holds m.mu.
func (m *Memory) sweep(now time.Time) {
	for k, w := range m.keys {
		if !now.Before(w.ends) {
			delete(m.keys, k)
		}
	}
}

// Len reports how many counters are held.
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.keys)
}
