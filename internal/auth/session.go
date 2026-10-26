package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"time"
)

const (
	// SessionTTL is how long a login lasts. After that the user logs in again.
	SessionTTL = 24 * time.Hour

	// tokenPrefix marks GoFlow session tokens, like GitHub's "ghp_". A token
	// pasted into a log or a chat is recognisable, and secret scanners can find it.
	tokenPrefix = "gfs_"

	// tokenBytes of randomness: 256 bits, far beyond guessing.
	tokenBytes = 32
)

// tokenLength is the prefix plus 32 bytes in unpadded base64url (43 characters).
var tokenLength = len(tokenPrefix) + base64.RawURLEncoding.EncodedLen(tokenBytes)

var ErrSessionNotFound = errors.New("auth: session not found or expired")

// Session is one login. Only the SHA-256 hash of the token is kept: whoever
// reads the sessions table still cannot log in as anyone.
type Session struct {
	TokenHash []byte
	UserID    string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// NewSession creates a session for userID that starts at now. It returns the
// token to give the client once, and the session to store.
func NewSession(userID string, now time.Time) (string, *Session) {
	b := make([]byte, tokenBytes)
	rand.Read(b)
	token := tokenPrefix + base64.RawURLEncoding.EncodeToString(b)

	now = now.UTC().Truncate(time.Microsecond)
	return token, &Session{
		TokenHash: HashToken(token),
		UserID:    userID,
		CreatedAt: now,
		ExpiresAt: now.Add(SessionTTL),
	}
}

// HashToken returns the SHA-256 of a token. A fast hash is right here, unlike
// for passwords: a random 256-bit token can't be guessed, however fast the hash.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// ValidTokenFormat rejects anything that can't be a GoFlow token before it
// costs a database query.
func ValidTokenFormat(token string) bool {
	return len(token) == tokenLength && strings.HasPrefix(token, tokenPrefix)
}
