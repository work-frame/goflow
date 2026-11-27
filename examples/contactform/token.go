package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

// tokens makes and checks submission tokens: 16 random bytes and an
// HMAC-SHA256 of them, under a key made fresh when the process starts.
//
// A token does two jobs. Its random part becomes GoFlow's Idempotency-Key,
// so sending the same page twice makes one job. And because only this
// process can sign one, a visitor can't choose the key (to collide with
// someone else's message), and another website can't post the form on a
// visitor's behalf: it can't read the token out of the page. Nothing is
// stored, so a restart makes every open page's token invalid ("reload").
type tokens struct{ key []byte }

const tokenIDBytes = 16

var b64 = base64.RawURLEncoding

func newTokens() *tokens {
	key := make([]byte, 32)
	rand.Read(key) // never returns an error (crypto/rand, Go 1.24+)
	return &tokens{key: key}
}

func (t *tokens) sign(id []byte) []byte {
	m := hmac.New(sha256.New, t.key)
	m.Write(id)
	return m.Sum(nil)
}

// issue returns a new token: base64url(id) "." base64url(HMAC(id)).
func (t *tokens) issue() string {
	id := make([]byte, tokenIDBytes)
	rand.Read(id)
	return b64.EncodeToString(id) + "." + b64.EncodeToString(t.sign(id))
}

// check returns the token's ID if this process signed it.
func (t *tokens) check(token string) (id string, ok bool) {
	idPart, macPart, found := strings.Cut(token, ".")
	if !found {
		return "", false
	}
	rawID, err1 := b64.DecodeString(idPart)
	mac, err2 := b64.DecodeString(macPart)
	if err1 != nil || err2 != nil || len(rawID) != tokenIDBytes {
		return "", false
	}
	if !hmac.Equal(mac, t.sign(rawID)) {
		return "", false
	}
	return idPart, true
}
