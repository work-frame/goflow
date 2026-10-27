package auth

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestNewSession(t *testing.T) {
	now := time.Date(2026, 10, 26, 9, 0, 0, 123456789, time.UTC)
	token, s := NewSession("user-1", now)

	if !strings.HasPrefix(token, "gfs_") || len(token) != 47 {
		t.Errorf("token = %q, want gfs_ + 43 characters", token)
	}
	if !ValidTokenFormat(token) {
		t.Errorf("ValidTokenFormat(%q) = false for a fresh token", token)
	}
	if !bytes.Equal(s.TokenHash, HashToken(token)) {
		t.Error("session stores something other than the token's hash")
	}
	if bytes.Contains(s.TokenHash, []byte(token)) {
		t.Error("session contains the raw token")
	}
	if s.UserID != "user-1" {
		t.Errorf("UserID = %q, want user-1", s.UserID)
	}
	if want := now.Truncate(time.Microsecond); !s.CreatedAt.Equal(want) {
		t.Errorf("CreatedAt = %v, want %v", s.CreatedAt, want)
	}
	if got := s.ExpiresAt.Sub(s.CreatedAt); got != SessionTTL {
		t.Errorf("session lasts %v, want %v", got, SessionTTL)
	}
}

func TestTokensAreUnique(t *testing.T) {
	seen := make(map[string]bool)
	for range 1000 {
		token, _ := NewSession("u", time.Now())
		if seen[token] {
			t.Fatalf("token %q generated twice", token)
		}
		seen[token] = true
	}
}

func TestValidTokenFormat(t *testing.T) {
	good, _ := NewSession("u", time.Now())
	tests := map[string]bool{
		good:                               true,
		"":                                 false,
		"gfs_short":                        false,
		"xyz_" + good[4:]:                  false,
		good + "a":                         false,
		strings.Replace(good, "_", "-", 1): false,
	}
	for token, want := range tests {
		if got := ValidTokenFormat(token); got != want {
			t.Errorf("ValidTokenFormat(%q) = %v, want %v", token, got, want)
		}
	}
}

func TestNeedsRehash(t *testing.T) {
	old, err := Hasher{Cost: bcrypt.MinCost}.Hash("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !(Hasher{Cost: bcrypt.MinCost + 1}).NeedsRehash(old) {
		t.Error("a cost-4 hash should need rehashing at cost 5")
	}
	if (Hasher{Cost: bcrypt.MinCost}).NeedsRehash(old) {
		t.Error("a hash at the current cost should not need rehashing")
	}
	newer, err := Hasher{Cost: bcrypt.MinCost + 1}.Hash("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !(Hasher{Cost: bcrypt.MinCost}).NeedsRehash(newer) {
		t.Error("a cost-5 hash should be rehashed at cost 4 too: every hash at one cost")
	}
	if (Hasher{}).NeedsRehash(mustHashAt(t, bcrypt.DefaultCost)) {
		t.Error("the zero Hasher hashes at bcrypt's default cost, so that cost is current")
	}
	if (Hasher{Cost: bcrypt.MinCost + 1}).NeedsRehash("not a hash") {
		t.Error("a malformed hash should not be reported as upgradable")
	}
}

func mustHashAt(t *testing.T, cost int) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte("correct horse battery"), cost)
	if err != nil {
		t.Fatal(err)
	}
	return string(h)
}
