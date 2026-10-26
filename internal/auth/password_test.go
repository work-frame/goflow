package auth

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

var testHasher = Hasher{Cost: bcrypt.MinCost}

func TestValidatePassword(t *testing.T) {
	tests := []struct {
		name     string
		password string
		want     error
	}{
		{"11 chars", strings.Repeat("a", 11), ErrPasswordTooShort},
		{"12 chars", strings.Repeat("a", 12), nil},
		{"72 bytes", strings.Repeat("a", 72), nil},
		{"73 bytes", strings.Repeat("a", 73), ErrPasswordTooLong},
		{"12 runes, 48 bytes", strings.Repeat("😀", 12), nil},
		{"19 runes, 76 bytes", strings.Repeat("😀", 19), ErrPasswordTooLong},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidatePassword(tt.password); !errors.Is(err, tt.want) {
				t.Fatalf("ValidatePassword() = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestHashAndCompare(t *testing.T) {
	const password = "correct horse battery"

	hash, err := testHasher.Hash(password)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if strings.Contains(hash, password) {
		t.Fatal("hash contains the plain password")
	}
	if err := testHasher.Compare(hash, password); err != nil {
		t.Fatalf("Compare(right password) = %v, want nil", err)
	}
	if err := testHasher.Compare(hash, "wrong horse battery"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("Compare(wrong password) = %v, want ErrWrongPassword", err)
	}
}

func TestHashIsSalted(t *testing.T) {
	a, err := testHasher.Hash("same password twice")
	if err != nil {
		t.Fatal(err)
	}
	b, err := testHasher.Hash("same password twice")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two hashes of the same password are equal: no salt")
	}
}

func TestHashUsesCost(t *testing.T) {
	hash, err := Hasher{Cost: 5}.Hash("cost check password")
	if err != nil {
		t.Fatal(err)
	}
	cost, err := bcrypt.Cost([]byte(hash))
	if err != nil {
		t.Fatal(err)
	}
	if cost != 5 {
		t.Fatalf("cost = %d, want 5", cost)
	}
}

func TestHashRejectsInvalidPassword(t *testing.T) {
	if _, err := testHasher.Hash("short"); !errors.Is(err, ErrPasswordTooShort) {
		t.Fatalf("Hash(short) = %v, want ErrPasswordTooShort", err)
	}
}

func TestCompareMalformedHash(t *testing.T) {
	err := testHasher.Compare("not-a-bcrypt-hash", "whatever password")
	if err == nil || errors.Is(err, ErrWrongPassword) {
		t.Fatalf("Compare(malformed) = %v, want a non-mismatch error", err)
	}
}

// Run with: go test -run '^$' -bench . ./internal/auth/
func BenchmarkHashDefaultCost(b *testing.B) {
	h := Hasher{Cost: DefaultCost}
	for b.Loop() {
		if _, err := h.Hash("benchmark password"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHashMinCost(b *testing.B) {
	for b.Loop() {
		if _, err := testHasher.Hash("benchmark password"); err != nil {
			b.Fatal(err)
		}
	}
}
