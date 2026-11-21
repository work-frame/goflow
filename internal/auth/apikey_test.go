package auth

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNewAPIKey(t *testing.T) {
	now := time.Date(2026, 11, 22, 10, 0, 0, 123456789, time.UTC)
	secret, k, err := NewAPIKey("u1", "  contact form  ", []Scope{ScopeJobsRead, ScopeJobsCreate, ScopeJobsRead}, 90, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, "gfk_") || len(secret) != 47 || !ValidKeyFormat(secret) {
		t.Errorf("secret %q: want gfk_ and 43 base64url characters", secret)
	}
	if !bytes.Equal(k.KeyHash, HashToken(secret)) || len(k.KeyHash) != 32 {
		t.Error("the stored hash isn't the secret's SHA-256")
	}
	if k.Hint != secret[43:] || len(k.Hint) != 4 {
		t.Errorf("hint %q, want the secret's last 4 characters", k.Hint)
	}
	if k.Name != "contact form" || k.UserID != "u1" || len(k.ID) != 32 {
		t.Errorf("name %q user %q id %q", k.Name, k.UserID, k.ID)
	}
	if len(k.Scopes) != 2 || k.Scopes[0] != ScopeJobsCreate || k.Scopes[1] != ScopeJobsRead {
		t.Errorf("scopes %v, want [jobs:create jobs:read], each once", k.Scopes)
	}
	if want := now.Truncate(time.Microsecond).AddDate(0, 0, 90); !k.ExpiresAt.Equal(want) {
		t.Errorf("expires %v, want %v", k.ExpiresAt, want)
	}
	other, _, _ := NewAPIKey("u1", "x", []Scope{ScopeJobsCreate}, 30, now)
	if other == secret {
		t.Error("two keys came out the same")
	}
}

func TestNewAPIKeyRefusesBadRequests(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name   string
		key    string
		scopes []Scope
		days   int
		want   error
	}{
		{"empty name", "   ", []Scope{ScopeJobsCreate}, 90, ErrInvalidKeyName},
		{"long name", strings.Repeat("é", 101), []Scope{ScopeJobsCreate}, 90, ErrInvalidKeyName},
		{"no scopes", "k", nil, 90, ErrInvalidScopes},
		{"unknown scope", "k", []Scope{ScopeJobsCreate, "jobs:delete"}, 90, ErrInvalidScopes},
		{"admin scope", "k", []Scope{"*"}, 90, ErrInvalidScopes},
		{"0 days", "k", []Scope{ScopeJobsCreate}, 0, ErrInvalidLifetime},
		{"never", "k", []Scope{ScopeJobsCreate}, -1, ErrInvalidLifetime},
		{"60 days", "k", []Scope{ScopeJobsCreate}, 60, ErrInvalidLifetime},
	}
	for _, c := range cases {
		if _, _, err := NewAPIKey("u", c.key, c.scopes, c.days, now); !errors.Is(err, c.want) {
			t.Errorf("%s: err %v, want %v", c.name, err, c.want)
		}
	}
	if _, _, err := NewAPIKey("u", strings.Repeat("é", 100), []Scope{ScopeJobsRead}, 365, now); err != nil {
		t.Errorf("100-character name refused: %v", err)
	}
}

// Deny by default: a key allows exactly the scopes it was given.
func TestAPIKeyAllowsOnlyItsScopes(t *testing.T) {
	k := &APIKey{Scopes: []Scope{ScopeJobsCreate}}
	if !k.Allows(ScopeJobsCreate) || k.Allows(ScopeJobsRead) || k.Allows("") {
		t.Error("a jobs:create key must allow jobs:create and nothing else")
	}
	if (&APIKey{}).Allows(ScopeJobsCreate) || (&APIKey{}).Allows("") {
		t.Error("a key with no scopes must allow nothing")
	}
}

func TestValidKeyFormat(t *testing.T) {
	secret, _, _ := NewAPIKey("u", "k", []Scope{ScopeJobsCreate}, 30, time.Now())
	session, _ := NewSession("u", time.Now())
	for _, bad := range []string{"", "gfk_", session, "gfk_" + strings.Repeat("A", 42), "gfk_" + strings.Repeat("A", 44),
		"gfk_" + strings.Repeat("A", 42) + "=", "gfk_" + strings.Repeat("A", 42) + "+", "GFK_" + secret[4:]} {
		if ValidKeyFormat(bad) {
			t.Errorf("ValidKeyFormat(%q) = true", bad)
		}
	}
	if !ValidKeyFormat(secret) || !IsKey(secret) || IsKey(session) {
		t.Error("a real key must be valid, and a session token must not look like a key")
	}
}
