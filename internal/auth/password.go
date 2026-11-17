// Package auth holds the rules for user credentials: email format and
// password hashing. It knows nothing about HTTP or SQL.
package auth

import (
	"errors"
	"fmt"
	"strconv"
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
	// MinSettingCost and MaxSettingCost bound GOFLOW_BCRYPT_COST. Below 10,
	// stolen hashes are too cheap to crack. Each step doubles the time, so
	// 14 is already about 7 s per login on a laptop.
	MinSettingCost = 10
	MaxSettingCost = 14
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

// NeedsRehash reports whether hash was made with a different cost than h uses,
// higher or lower. Login is the only time the server sees the password again,
// so that is when the hash is brought to the configured cost. Keeping every
// hash at one cost also keeps login timing the same for every account.
func (h Hasher) NeedsRehash(hash string) bool {
	cost, err := bcrypt.Cost([]byte(hash))
	return err == nil && cost != h.effectiveCost()
}

// effectiveCost is the cost Hash really uses: bcrypt replaces anything below
// its minimum with its default.
func (h Hasher) effectiveCost() int {
	if h.Cost < bcrypt.MinCost {
		return bcrypt.DefaultCost
	}
	return h.Cost
}

// CostSetting reads GOFLOW_BCRYPT_COST: DefaultCost when raw is empty,
// otherwise a whole number from MinSettingCost to MaxSettingCost. The API
// and goflow-admin both use it, so they can never disagree on the rule.
func CostSetting(raw string) (int, error) {
	if raw == "" {
		return DefaultCost, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("GOFLOW_BCRYPT_COST must be a whole number, got %q", raw)
	}
	if n < MinSettingCost || n > MaxSettingCost {
		return 0, fmt.Errorf("GOFLOW_BCRYPT_COST must be between %d and %d, got %d", MinSettingCost, MaxSettingCost, n)
	}
	return n, nil
}
