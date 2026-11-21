package auth

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// API keys (Task 86) let another program, such as a website's backend, call
// GoFlow without a person logging in. A key is a bearer token like a
// session token, with three differences: it lasts 30, 90 or 365 days, it
// can do only what its scopes allow, and it never reaches account routes.
const (
	// keyPrefix marks GoFlow API keys, as gfs_ marks session tokens.
	keyPrefix = "gfk_"

	// MaxKeyNameLength is the longest name, in characters.
	MaxKeyNameLength = 100

	// MaxActiveKeys is how many unrevoked, unexpired keys one account may
	// hold. Revoked and expired keys don't count; they stay listed.
	MaxActiveKeys = 10

	// keyHintLength is how many of the key's last characters are kept in
	// the clear, so a person can tell keys apart ("gfk_…x7Qa"). 4 of 43
	// characters leave 234 bits unknown.
	keyHintLength = 4
)

// Scope is one thing a key may do. Every route a key may use names the
// scope it needs; a route that names none is closed to keys.
type Scope string

const (
	// ScopeJobsCreate allows POST /api/v1/jobs.
	ScopeJobsCreate Scope = "jobs:create"
	// ScopeJobsRead allows listing and reading the owner's jobs and their attempts.
	ScopeJobsRead Scope = "jobs:read"
)

// Scopes is every scope a key can be given, in the order they are listed.
var Scopes = []Scope{ScopeJobsCreate, ScopeJobsRead}

// KeyLifetimes are the lifetimes a key may be created with, in days.
var KeyLifetimes = []int{30, 90, 365}

// Key statuses, as the database works them out on its own clock.
const (
	KeyActive  = "active"
	KeyExpired = "expired"
	KeyRevoked = "revoked"
)

var (
	ErrKeyNotFound     = errors.New("auth: API key not found, expired or revoked")
	ErrInvalidKeyName  = errors.New("auth: key name must be 1 to 100 characters")
	ErrInvalidScopes   = errors.New("auth: key scopes must be one or more of jobs:create, jobs:read")
	ErrInvalidLifetime = errors.New("auth: key lifetime must be 30, 90 or 365 days")
	ErrTooManyKeys     = errors.New("auth: the account already has the maximum number of active keys")
)

// APIKey is one key. Only the SHA-256 of the secret is kept; the secret
// itself is shown once, when the key is made.
type APIKey struct {
	ID         string
	UserID     string
	Name       string
	KeyHash    []byte
	Hint       string
	Scopes     []Scope
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
	// Status is KeyActive, KeyExpired or KeyRevoked, as of the database's
	// clock when the key was read. Empty on a key that hasn't been stored.
	Status string
}

// Allows reports whether the key has scope s. A key with no scopes allows
// nothing: there is no default.
func (k *APIKey) Allows(s Scope) bool {
	return slices.Contains(k.Scopes, s)
}

// NewAPIKey checks the request and makes a key for userID that starts at
// now. It returns the secret to give the client once, and the key to store.
func NewAPIKey(userID, name string, scopes []Scope, lifetimeDays int, now time.Time) (string, *APIKey, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > MaxKeyNameLength {
		return "", nil, ErrInvalidKeyName
	}
	clean, err := checkScopes(scopes)
	if err != nil {
		return "", nil, err
	}
	if !slices.Contains(KeyLifetimes, lifetimeDays) {
		return "", nil, ErrInvalidLifetime
	}

	b := make([]byte, tokenBytes)
	rand.Read(b)
	secret := keyPrefix + base64.RawURLEncoding.EncodeToString(b)

	now = now.UTC().Truncate(time.Microsecond)
	return secret, &APIKey{
		ID:        newID(),
		UserID:    userID,
		Name:      name,
		KeyHash:   HashToken(secret),
		Hint:      secret[len(secret)-keyHintLength:],
		Scopes:    clean,
		CreatedAt: now,
		ExpiresAt: now.AddDate(0, 0, lifetimeDays),
	}, nil
}

// checkScopes refuses an empty list and any unknown scope, and returns the
// scopes once each, in the order of Scopes.
func checkScopes(scopes []Scope) ([]Scope, error) {
	if len(scopes) == 0 {
		return nil, ErrInvalidScopes
	}
	for _, s := range scopes {
		if !slices.Contains(Scopes, s) {
			return nil, ErrInvalidScopes
		}
	}
	var clean []Scope
	for _, s := range Scopes {
		if slices.Contains(scopes, s) {
			clean = append(clean, s)
		}
	}
	return clean, nil
}

// IsKey reports whether a bearer token claims to be an API key (starts
// with gfk_). It says nothing about whether the key is real.
func IsKey(token string) bool {
	return strings.HasPrefix(token, keyPrefix)
}

// ValidKeyFormat rejects anything that can't be a GoFlow API key before it
// costs a database query: gfk_ and 43 characters of base64url.
func ValidKeyFormat(token string) bool {
	if len(token) != tokenLength || !IsKey(token) {
		return false
	}
	_, err := base64.RawURLEncoding.Strict().DecodeString(token[len(keyPrefix):])
	return err == nil
}
