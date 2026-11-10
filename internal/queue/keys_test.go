package queue

import "testing"

// Production's key names don't move when tests use their own prefix: Keys
// with DefaultPrefix gives exactly the names cmd/worker, the API and every
// running deployment already use. Literals, not the constants, so a renamed
// constant fails here too (Task 54).
func TestDefaultKeysAreTheProductionNames(t *testing.T) {
	pending, processing, delayed, leases := Keys(DefaultPrefix)
	got := []string{pending, processing, delayed, leases, PendingKey, ProcessingKey, DelayedKey, LeasesKey}
	want := []string{"goflow:jobs:pending", "goflow:jobs:processing", "goflow:jobs:delayed", "goflow:jobs:leases",
		"goflow:jobs:pending", "goflow:jobs:processing", "goflow:jobs:delayed", "goflow:jobs:leases"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("key %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// WithPrefix moves all four keys, and only for the copy it returns.
func TestWithPrefixMovesEveryKey(t *testing.T) {
	base := &Queue{key: PendingKey, processing: ProcessingKey, delayed: DelayedKey, leases: LeasesKey}
	q := base.WithPrefix("goflow:test:x:")
	if q.key != "goflow:test:x:jobs:pending" || q.processing != "goflow:test:x:jobs:processing" ||
		q.delayed != "goflow:test:x:jobs:delayed" || q.leases != "goflow:test:x:jobs:leases" {
		t.Errorf("WithPrefix keys = %q %q %q %q", q.key, q.processing, q.delayed, q.leases)
	}
	if base.key != PendingKey || base.leases != LeasesKey {
		t.Error("WithPrefix changed the queue it was called on")
	}
}
