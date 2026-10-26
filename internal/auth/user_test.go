package auth

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNewUser(t *testing.T) {
	u, err := NewUser("  Dan@Example.com ", "correct horse battery", testHasher)
	if err != nil {
		t.Fatalf("NewUser: %v", err)
	}
	if u.Email != "dan@example.com" {
		t.Errorf("Email = %q, want dan@example.com", u.Email)
	}
	if len(u.ID) != 32 {
		t.Errorf("ID = %q, want 32 hex characters", u.ID)
	}
	if err := testHasher.Compare(u.PasswordHash, "correct horse battery"); err != nil {
		t.Errorf("stored hash does not match the password: %v", err)
	}
	if u.CreatedAt.Location() != time.UTC || u.CreatedAt.Nanosecond()%1000 != 0 {
		t.Errorf("CreatedAt = %v, want UTC truncated to microseconds", u.CreatedAt)
	}
}

func TestNewUserRejectsBadInput(t *testing.T) {
	tests := []struct {
		name, email, password string
		want                  error
	}{
		{"bad email", "not-an-email", "correct horse battery", ErrInvalidEmail},
		{"short password", "dan@example.com", "short", ErrPasswordTooShort},
		{"long password", "dan@example.com", strings.Repeat("a", 73), ErrPasswordTooLong},
		{"email checked first", "not-an-email", "short", ErrInvalidEmail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewUser(tt.email, tt.password, testHasher); !errors.Is(err, tt.want) {
				t.Fatalf("NewUser() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestUserJSONHasNoPasswordHash(t *testing.T) {
	u, err := NewUser("dan@example.com", "correct horse battery", testHasher)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "password") || strings.Contains(string(b), u.PasswordHash) {
		t.Fatalf("JSON leaks the password hash: %s", b)
	}
}
