package secrets

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func newKey(fill byte) string {
	b := make([]byte, KeySize)
	for i := range b {
		b[i] = fill + byte(i)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func TestParseKeysRefusals(t *testing.T) {
	good := newKey(1)
	repeated := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, KeySize))
	short := base64.StdEncoding.EncodeToString(make([]byte, 16))
	for _, c := range []struct {
		name, current, old, want string
	}{
		{"missing", "", "", "GOFLOW_SECRETS_KEY is required"},
		{"not base64", "not*base64", "", "GOFLOW_SECRETS_KEY must be standard base64"},
		{"too short", short, "", "exactly 32 bytes, got 16"},
		{"repeated byte", repeated, "", "one byte repeated"},
		{"bad old key", good, newKey(9) + ",bad", "GOFLOW_SECRETS_OLD_KEYS entry 2 must be standard base64"},
	} {
		_, err := ParseKeys(c.current, c.old)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", c.name, err, c.want)
		}
		if err != nil && c.current != "" && strings.Contains(err.Error(), strings.TrimSpace(c.current)) {
			t.Errorf("%s: the error repeats the key", c.name)
		}
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	kr, err := ParseKeys(newKey(1), "")
	if err != nil {
		t.Fatal(err)
	}
	s := kr.Seal([]byte("whsec_abc"), []byte("user-a"))
	if s.KeyID != kr.CurrentID() || len(s.KeyID) != 8 {
		t.Errorf("sealed under %q, want the current key's 8-character ID %q", s.KeyID, kr.CurrentID())
	}
	p, err := kr.Open(s, []byte("user-a"))
	if err != nil || string(p) != "whsec_abc" {
		t.Fatalf("Open = %q, %v", p, err)
	}
	if again := kr.Seal([]byte("whsec_abc"), []byte("user-a")); bytes.Equal(again.Nonce, s.Nonce) {
		t.Error("two seals share a nonce")
	}
}

// The answer to the Task 65 question: a ciphertext copied onto another
// owner's row doesn't decrypt there (FAILURES.md, case 60).
func TestACopiedSecretDoesNotOpenForAnotherOwner(t *testing.T) {
	kr, _ := ParseKeys(newKey(1), "")
	s := kr.Seal([]byte("whsec_alice"), []byte("user-alice"))
	if p, err := kr.Open(s, []byte("user-bob")); !errors.Is(err, ErrTampered) {
		t.Errorf("Open with bob's ID = %q, %v; want ErrTampered", p, err)
	}
	s.Ciphertext[0] ^= 1
	if _, err := kr.Open(s, []byte("user-alice")); !errors.Is(err, ErrTampered) {
		t.Errorf("Open of modified bytes: %v, want ErrTampered", err)
	}
}

// Rotation: the new key encrypts, the old one still decrypts what it
// sealed, and without it the old ciphertext is reported, not garbled.
func TestOldKeysOnlyDecrypt(t *testing.T) {
	oldKey, newK := newKey(1), newKey(50)
	before, _ := ParseKeys(oldKey, "")
	sealed := before.Seal([]byte("whsec_x"), []byte("u"))

	during, err := ParseKeys(newK, oldKey)
	if err != nil {
		t.Fatal(err)
	}
	if during.CurrentID() == before.CurrentID() {
		t.Fatal("two different keys got the same ID")
	}
	if p, err := during.Open(sealed, []byte("u")); err != nil || string(p) != "whsec_x" {
		t.Errorf("old ciphertext with the old key loaded: %q, %v", p, err)
	}
	if during.Seal([]byte("y"), []byte("u")).KeyID != during.CurrentID() {
		t.Error("Seal didn't use the new key")
	}

	after, _ := ParseKeys(newK, "")
	if _, err := after.Open(sealed, []byte("u")); !errors.Is(err, ErrUnknownKey) || !strings.Contains(err.Error(), before.CurrentID()) {
		t.Errorf("old ciphertext without the old key: %v, want ErrUnknownKey naming %s", err, before.CurrentID())
	}
}

func TestNewSigningSecret(t *testing.T) {
	s := NewSigningSecret()
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, "whsec_"))
	if !strings.HasPrefix(s, "whsec_") || err != nil || len(raw) != 32 {
		t.Errorf("secret %q: want whsec_ and 32 bytes of base64", s)
	}
	if NewSigningSecret() == s {
		t.Error("two secrets are equal")
	}
}
