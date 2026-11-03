package job

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateIdempotencyKey(t *testing.T) {
	good := []string{"a", "7f3c1e9a-0b2d-4c55-9e1f-2a3b4c5d6e7f", "invoice:42/retry", strings.Repeat("k", MaxIdempotencyKeyLength)}
	for _, k := range good {
		if err := ValidateIdempotencyKey(k); err != nil {
			t.Errorf("ValidateIdempotencyKey(%q) = %v, want nil", k, err)
		}
	}
	bad := []string{"", " ", "a b", "line\nbreak", "\x7f", "clé", strings.Repeat("k", MaxIdempotencyKeyLength+1)}
	for _, k := range bad {
		if err := ValidateIdempotencyKey(k); !errors.Is(err, ErrBadIdempotencyKey) {
			t.Errorf("ValidateIdempotencyKey(%q) = %v, want ErrBadIdempotencyKey", k, err)
		}
	}
}

func TestOnlyDeadAndFailedAreRerunnable(t *testing.T) {
	for _, s := range []Status{StatusPending, StatusQueued, StatusProcessing, StatusRetrying, StatusCompleted, StatusFailed, StatusDead} {
		want := s == StatusDead || s == StatusFailed
		if got := s.Rerunnable(); got != want {
			t.Errorf("%s.Rerunnable() = %v, want %v", s, got, want)
		}
	}
}

func TestMemoryStoreRerun(t *testing.T) {
	s := NewMemoryStore()
	j, _ := New("x")
	j.Status, j.Attempts = StatusDead, 3
	if err := s.Add(j); err != nil {
		t.Fatal(err)
	}
	got, err := s.Rerun(j.ID)
	if err != nil || got.Status != StatusPending || got.Attempts != 0 {
		t.Fatalf("Rerun = %+v, %v; want pending with 0 attempts", got, err)
	}
	if _, err := s.Rerun(j.ID); !errors.Is(err, ErrNotRerunnable) {
		t.Errorf("second Rerun = %v, want ErrNotRerunnable", err)
	}
	if _, err := s.Rerun("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Rerun(missing) = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreIdempotencyKeys(t *testing.T) {
	s := NewMemoryStore()
	add := func(owner, key string) error {
		j, _ := New("x")
		j.UserID, j.IdempotencyKey = owner, key
		return s.Add(j)
	}
	if err := add("alice", "k"); err != nil {
		t.Fatal(err)
	}
	if err := add("alice", "k"); !errors.Is(err, ErrDuplicateKey) {
		t.Errorf("same owner, same key: %v, want ErrDuplicateKey", err)
	}
	if err := add("bob", "k"); err != nil {
		t.Errorf("other owner, same key: %v, want nil", err)
	}
	if err := add("alice", ""); err != nil {
		t.Fatal(err)
	}
	if err := add("alice", ""); err != nil {
		t.Errorf("two jobs without a key: %v, want nil", err)
	}
}
