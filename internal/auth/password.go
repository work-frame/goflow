// Package auth holds the rules for user credentials: email format and
// password hashing. It knows nothing about HTTP or SQL.
package auth

import (
	"errors"
	"fmt"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

const (
	// MinPasswordLength is counted in characters (runes).
	MinPasswordLength = 12
	// MaxPasswordBytes is bcrypt's hard limit. It is counted in bytes,
	// so 72 ASCII characters fit but only 18 four-byte emoji do.
	MaxPasswordBytes = 72
	// DefaultCost makes one hash take roughly a quarter of a second.
	DefaultCost = 12
)

var (
	ErrPasswordTooShort = errors.New("auth: password must be at least 12 characters")
	ErrPasswordTooLong  = errors.New("auth: password must be at most 72 bytes")
	ErrWrongPassword    = errors.New("auth: wrong password")
)

// Hasher hashes passwords with bcrypt. Production uses DefaultCost;
// tests use bcrypt.MinCost so the race detector run stays fast.
type Hasher struct {
	Cost int
}

// ValidatePassword checks length only. Composition rules ("one digit,
// one symbol") push people toward Password1! and add little security.
func ValidatePassword(password string) error {
	if utf8.RuneCountInString(password) < MinPasswordLength {
		return ErrPasswordTooShort
	}
	if len(password) > MaxPasswordBytes {
		return ErrPasswordTooLong
	}
	return nil
}

// Hash returns a bcrypt hash string. The salt and cost are stored inside
// the string, so the same password hashes differently every time.
func (h Hasher) Hash(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	b, err := bcrypt.GenerateFromPassword([]byte(password), h.Cost)
	if err != nil {
		return "", fmt.Errorf("auth: hash password: %w", err)
	}
	return string(b), nil
}

// Compare reports whether password matches hash. It returns
// ErrWrongPassword on a mismatch and a wrapped error if the hash itself
// is malformed. bcrypt compares in constant time.
func (Hasher) Compare(hash, password string) error {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return ErrWrongPassword
	}
	if err != nil {
		return fmt.Errorf("auth: compare password: %w", err)
	}
	return nil
}

// NeedsRehash reports whether hash was made with a lower cost than h uses.
// Login is the only time the server sees the password again, so that is
// when an old, cheaper hash can be upgraded.
func (h Hasher) NeedsRehash(hash string) bool {
	cost, err := bcrypt.Cost([]byte(hash))
	return err == nil && cost < h.Cost
}
