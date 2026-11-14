// Package secrets encrypts small secrets that GoFlow must read back, such
// as webhook signing secrets, with the master key GOFLOW_SECRETS_KEY.
//
// Each ciphertext is AES-256-GCM under one key, named by that key's ID: the
// first 8 hex characters of the key's SHA-256. The ID is computed, never
// typed. A Keyring holds the current key, which encrypts, and any old keys
// (GOFLOW_SECRETS_OLD_KEYS), which only decrypt, so a rotation never makes
// a stored secret unreadable. Every ciphertext is bound to a context, such
// as its owner's user ID (additional authenticated data): moved to another
// owner's row, it no longer decrypts.
package secrets

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// KeySize is the master key's length: 32 bytes, AES-256.
const KeySize = 32

// ErrUnknownKey means a ciphertext was made with a key the Keyring doesn't
// hold: it can't be read until that key is given back.
var ErrUnknownKey = errors.New("secrets: encrypted with a key that isn't loaded")

// ErrTampered means decryption failed authentication: wrong context (moved
// to another owner), modified bytes, or a different key with the same ID.
var ErrTampered = errors.New("secrets: ciphertext doesn't authenticate")

type key struct {
	id   string
	aead cipher.AEAD
}

// Keyring holds the current key and the old ones.
type Keyring struct {
	current key
	byID    map[string]key
}

// ParseKeys builds a Keyring from GOFLOW_SECRETS_KEY (current) and
// GOFLOW_SECRETS_OLD_KEYS (comma-separated, may be empty). Errors name the
// variable and the position, never the value.
func ParseKeys(current, old string) (*Keyring, error) {
	if strings.TrimSpace(current) == "" {
		return nil, errors.New("GOFLOW_SECRETS_KEY is required: base64 of 32 random bytes (Render generates it; see DEPLOYMENT.md)")
	}
	cur, err := parseKey(current)
	if err != nil {
		return nil, fmt.Errorf("GOFLOW_SECRETS_KEY %w", err)
	}
	kr := &Keyring{current: cur, byID: map[string]key{cur.id: cur}}
	if strings.TrimSpace(old) != "" {
		for i, raw := range strings.Split(old, ",") {
			k, err := parseKey(raw)
			if err != nil {
				return nil, fmt.Errorf("GOFLOW_SECRETS_OLD_KEYS entry %d %w", i+1, err)
			}
			kr.byID[k.id] = k
		}
	}
	return kr, nil
}

func parseKey(raw string) (key, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return key{}, errors.New("must be standard base64")
	}
	if len(b) != KeySize {
		return key{}, fmt.Errorf("must decode to exactly %d bytes, got %d", KeySize, len(b))
	}
	if bytes.Count(b, b[:1]) == len(b) {
		return key{}, errors.New("is one byte repeated: generate a random key")
	}
	block, err := aes.NewCipher(b)
	if err != nil {
		return key{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return key{}, err
	}
	sum := sha256.Sum256(b)
	return key{id: hex.EncodeToString(sum[:4]), aead: aead}, nil
}

// CurrentID is the ID of the key Seal uses. Safe to log: it is a hash
// prefix, not the key.
func (k *Keyring) CurrentID() string { return k.current.id }

// Has reports whether the Keyring can decrypt what key id encrypted.
func (k *Keyring) Has(id string) bool {
	_, ok := k.byID[id]
	return ok
}

// Sealed is one encrypted value, as stored.
type Sealed struct {
	KeyID      string
	Nonce      []byte
	Ciphertext []byte
}

// Seal encrypts plaintext with the current key, bound to context.
func (k *Keyring) Seal(plaintext, context []byte) Sealed {
	nonce := make([]byte, k.current.aead.NonceSize())
	rand.Read(nonce) // never fails since Go 1.24
	return Sealed{KeyID: k.current.id, Nonce: nonce, Ciphertext: k.current.aead.Seal(nil, nonce, plaintext, context)}
}

// Open decrypts s, which must have been sealed with the same context.
func (k *Keyring) Open(s Sealed, context []byte) ([]byte, error) {
	kk, ok := k.byID[s.KeyID]
	if !ok {
		return nil, fmt.Errorf("%w (key %s)", ErrUnknownKey, s.KeyID)
	}
	if len(s.Nonce) != kk.aead.NonceSize() {
		return nil, ErrTampered
	}
	p, err := kk.aead.Open(nil, s.Nonce, s.Ciphertext, context)
	if err != nil {
		return nil, ErrTampered
	}
	return p, nil
}

// NewSigningSecret returns a Standard Webhooks signing secret: "whsec_"
// and 32 random bytes in base64.
func NewSigningSecret() string {
	b := make([]byte, 32)
	rand.Read(b)
	return "whsec_" + base64.StdEncoding.EncodeToString(b)
}
