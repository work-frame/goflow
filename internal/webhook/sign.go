// Package webhook delivers webhook jobs: one signed POST per attempt,
// through internal/egress, with the outcome classified and recorded.
//
// Signatures follow the Standard Webhooks spec (standardwebhooks.com), so a
// receiver can verify them with an existing library: HMAC-SHA256 over
// "<webhook-id>.<webhook-timestamp>.<body>", keyed with the secret's bytes,
// sent as "v1,<base64>". The webhook-id is the job ID, the same on every
// attempt, so a receiver that remembers the IDs it processed can drop a
// repeat: delivery is at least once, never exactly once.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
)

// Header names from the Standard Webhooks spec.
const (
	HeaderID        = "webhook-id"
	HeaderTimestamp = "webhook-timestamp"
	HeaderSignature = "webhook-signature"
)

// Sign returns the "v1,<base64>" signature of one delivery with secret, a
// "whsec_..." signing secret.
func Sign(secret, id string, timestamp int64, body []byte) (string, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil || len(key) == 0 {
		return "", errors.New("webhook: signing secret is not whsec_ and base64")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id))
	mac.Write([]byte("."))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

// SignatureHeader signs with every secret and joins the signatures with
// spaces, the spec's way to send several during a rotation.
func SignatureHeader(secrets []string, id string, timestamp int64, body []byte) (string, error) {
	sigs := make([]string, 0, len(secrets))
	for _, s := range secrets {
		sig, err := Sign(s, id, timestamp, body)
		if err != nil {
			return "", err
		}
		sigs = append(sigs, sig)
	}
	return strings.Join(sigs, " "), nil
}

// Verify reports whether header carries a valid signature for secret: what
// a receiver does. GoFlow uses it in tests and in the live check.
func Verify(secret, id string, timestamp int64, body []byte, header string) bool {
	want, err := Sign(secret, id, timestamp, body)
	if err != nil {
		return false
	}
	for _, sig := range strings.Fields(header) {
		if hmac.Equal([]byte(sig), []byte(want)) {
			return true
		}
	}
	return false
}
