package database

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/work-frame/goflow/internal/job"
	"github.com/work-frame/goflow/internal/secrets"
)

func testKeys(t *testing.T, fill byte, old ...byte) *secrets.Keyring {
	t.Helper()
	enc := func(f byte) string {
		b := make([]byte, secrets.KeySize)
		for i := range b {
			b[i] = f + byte(i)
		}
		return base64.StdEncoding.EncodeToString(b)
	}
	var olds []string
	for _, o := range old {
		olds = append(olds, enc(o))
	}
	kr, err := secrets.ParseKeys(enc(fill), strings.Join(olds, ","))
	if err != nil {
		t.Fatal(err)
	}
	return kr
}

func TestSigningSecretLifecycle(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	ctx := context.Background()
	s := NewSecretStore(f.db, testKeys(t, 1))

	if _, err := s.Signing(ctx, f.alice); !errors.Is(err, ErrNoSecret) {
		t.Fatalf("Signing before any secret: %v, want ErrNoSecret", err)
	}
	if has, _ := s.Has(ctx, f.alice); has {
		t.Fatal("Has before any secret = true")
	}

	first, until, err := s.Rotate(ctx, f.alice)
	if err != nil || !strings.HasPrefix(first, "whsec_") || !until.IsZero() {
		t.Fatalf("first Rotate = %q, %v, %v; want a whsec_ secret and no previous", first, until, err)
	}
	got, err := s.Signing(ctx, f.alice)
	if err != nil || len(got) != 1 || got[0] != first {
		t.Fatalf("Signing = %v, %v; want [first]", got, err)
	}

	// The plaintext is nowhere in the row.
	var stored []byte
	if err := f.db.QueryRowContext(ctx, `SELECT ciphertext FROM webhook_secrets WHERE user_id = $1`, f.alice).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte(first)) || bytes.Contains(stored, []byte(strings.TrimPrefix(first, "whsec_"))) {
		t.Error("the stored ciphertext contains the secret")
	}

	second, until, err := s.Rotate(ctx, f.alice)
	if err != nil || second == first {
		t.Fatalf("second Rotate = %q, %v", second, err)
	}
	if d := time.Until(until); d < 23*time.Hour || d > 25*time.Hour {
		t.Errorf("previous valid until %v, want about 24 h from now", until)
	}
	got, _ = s.Signing(ctx, f.alice)
	if len(got) != 2 || got[0] != second || got[1] != first {
		t.Fatalf("Signing during the overlap = %v, want [second first]", got)
	}

	// 24 hours later, by the database's clock: only the new one signs.
	if _, err := f.db.ExecContext(ctx, `UPDATE webhook_secrets SET rotated_at = now() - interval '24 hours 1 second' WHERE user_id = $1`, f.alice); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Signing(ctx, f.alice)
	if len(got) != 1 || got[0] != second {
		t.Errorf("Signing after the overlap = %v, want [second]", got)
	}

	// A third rotation drops the oldest at once: only one previous is kept.
	third, _, _ := s.Rotate(ctx, f.alice)
	got, _ = s.Signing(ctx, f.alice)
	if len(got) != 2 || got[0] != third || got[1] != second {
		t.Errorf("Signing after a third rotation = %v, want [third second]", got)
	}
}

// Someone who can write to the database copies alice's encrypted secret
// onto bob's row. It must not become bob's: the ciphertext is bound to
// alice's ID (FAILURES.md, case 60).
func TestACopiedSecretDoesNotDecryptForAnotherUser(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	ctx := context.Background()
	s := NewSecretStore(f.db, testKeys(t, 1))
	if _, _, err := s.Rotate(ctx, f.alice); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Rotate(ctx, f.bob); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx,
		`UPDATE webhook_secrets b SET key_id = a.key_id, nonce = a.nonce, ciphertext = a.ciphertext
		 FROM webhook_secrets a WHERE a.user_id = $1 AND b.user_id = $2`, f.alice, f.bob); err != nil {
		t.Fatal(err)
	}
	got, err := s.Signing(ctx, f.bob)
	if !errors.Is(err, secrets.ErrTampered) {
		t.Errorf("bob's Signing after the copy = %v, %v; want secrets.ErrTampered, not alice's secret", got, err)
	}
}

// Key rotation (Task 65, K4): the API's startup moves secrets to the new key
// while the old one is still loaded, and changes nothing without it.
func TestReencryptMovesSecretsToTheCurrentKey(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	ctx := context.Background()
	oldOnly := NewSecretStore(f.db, testKeys(t, 1))
	first, _, _ := oldOnly.Rotate(ctx, f.alice)
	second, _, _ := oldOnly.Rotate(ctx, f.alice) // first is now the previous one
	if _, _, err := oldOnly.Rotate(ctx, f.bob); err != nil {
		t.Fatal(err)
	}

	// The new key without the old one: refused, nothing changed.
	newOnly := NewSecretStore(f.db, testKeys(t, 50))
	n, err := newOnly.Reencrypt(ctx)
	var missing MissingKeysError
	if !errors.As(err, &missing) || n != 0 || missing[testKeys(t, 1).CurrentID()] != 3 {
		t.Fatalf("Reencrypt without the old key = %d, %v; want a MissingKeysError counting 3 secrets", n, err)
	}
	if got, err := oldOnly.Signing(ctx, f.alice); err != nil || got[0] != second {
		t.Fatalf("after the refused re-encryption the old key reads %v, %v", got, err)
	}

	// New key, old one kept for decrypting: both users move.
	both := NewSecretStore(f.db, testKeys(t, 50, 1))
	if n, err := both.Reencrypt(ctx); err != nil || n != 2 {
		t.Fatalf("Reencrypt = %d, %v; want 2 users moved", n, err)
	}
	if n, _ := both.Reencrypt(ctx); n != 0 {
		t.Errorf("a second Reencrypt moved %d, want 0", n)
	}
	got, err := newOnly.Signing(ctx, f.alice)
	if err != nil || len(got) != 2 || got[0] != second || got[1] != first {
		t.Errorf("with only the new key: %v, %v; want [second first]", got, err)
	}
	if _, err := newOnly.Reencrypt(ctx); err != nil {
		t.Errorf("Reencrypt with only the new key after the move: %v", err)
	}
}

func TestAttemptsAreRecordedAndOwnerOnly(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	ctx := context.Background()
	j := mustNewWebhookJob(t, f.alice)
	if err := f.s.Add(ctx, j); err != nil {
		t.Fatal(err)
	}
	a := NewAttemptStore(f.db)
	start := time.Now().UTC().Truncate(time.Millisecond)
	long := strings.Repeat("é", 400)
	if err := a.Record(ctx, j.ID, job.Attempt{Attempt: 1, StartedAt: start, DurationMS: 12, StatusCode: 503, Outcome: job.OutcomeRetry, Error: long}); err != nil {
		t.Fatal(err)
	}
	if err := a.Record(ctx, j.ID, job.Attempt{Attempt: 2, StartedAt: start, DurationMS: 8, Outcome: job.OutcomeDelivered}); err != nil {
		t.Fatal(err)
	}
	if err := a.Record(ctx, "00000000000000000000000000000000", job.Attempt{Attempt: 1, StartedAt: start, Outcome: job.OutcomeRetry}); err != nil {
		t.Errorf("Record for a deleted job: %v, want nil", err)
	}

	got, err := a.List(ctx, f.alice, j.ID)
	if err != nil || len(got) != 2 {
		t.Fatalf("List = %v, %v", got, err)
	}
	if got[0].StatusCode != 503 || got[0].Outcome != job.OutcomeRetry || len([]rune(got[0].Error)) != job.MaxAttemptError {
		t.Errorf("first attempt %+v, want 503 retry with the error cut to %d characters", got[0], job.MaxAttemptError)
	}
	if got[1].StatusCode != 0 || got[1].Outcome != job.OutcomeDelivered || !got[1].StartedAt.Equal(start) {
		t.Errorf("second attempt %+v", got[1])
	}
	if _, err := a.List(ctx, f.bob, j.ID); !errors.Is(err, job.ErrNotFound) {
		t.Errorf("bob listing alice's attempts: %v, want ErrNotFound", err)
	}
}
