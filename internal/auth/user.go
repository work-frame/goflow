package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

var (
	ErrEmailTaken   = errors.New("auth: email already registered")
	ErrUserNotFound = errors.New("auth: user not found")
)

// User is an account that can own jobs. PasswordHash is a bcrypt hash,
// never the password itself, and it is never sent in a response.
type User struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
}

// NewUser validates the email and password and returns a user ready to store.
// The cheap email check runs before the slow hash.
func NewUser(email, password string, h Hasher) (*User, error) {
	normalized, err := NormalizeEmail(email)
	if err != nil {
		return nil, err
	}
	hash, err := h.Hash(password)
	if err != nil {
		return nil, err
	}
	return &User{
		ID:           newID(),
		Email:        normalized,
		PasswordHash: hash,
		// PostgreSQL stores microseconds; truncating here keeps round trips equal.
		CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
	}, nil
}

// newID returns 16 random bytes as a 32-character hex string, like job IDs.
func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
