package auth

import (
	"errors"
	"net/mail"
	"strings"
)

// MaxEmailLength is the practical limit from RFC 5321 (254 octets).
const MaxEmailLength = 254

var ErrInvalidEmail = errors.New("auth: invalid email address")

// NormalizeEmail trims and lowercases an address and checks that it is a
// bare address ("dan@example.com", not "Dan <dan@example.com>").
// After this, Dan@X.com and dan@x.com are the same account.
func NormalizeEmail(raw string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(raw))
	if email == "" || len(email) > MaxEmailLength {
		return "", ErrInvalidEmail
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return "", ErrInvalidEmail
	}
	return email, nil
}
