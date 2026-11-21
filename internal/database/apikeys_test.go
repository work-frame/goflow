package database

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/work-frame/goflow/internal/auth"
	"github.com/work-frame/goflow/internal/job"
)

// newKey makes and stores a key for userID that started at start.
func newKey(t *testing.T, s *APIKeyStore, userID string, scopes []auth.Scope, days int, start time.Time) (string, *auth.APIKey) {
	t.Helper()
	secret, k, err := auth.NewAPIKey(userID, "test key", scopes, days, start)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(context.Background(), k); err != nil {
		t.Fatalf("create key: %v", err)
	}
	return secret, k
}

var createOnly = []auth.Scope{auth.ScopeJobsCreate}

func TestAPIKeyStoreCreateAndLookup(t *testing.T) {
	t.Parallel()
	db, dan, _ := newSessionFixture(t)
	s := NewAPIKeyStore(db)
	ctx := context.Background()

	secret, k := newKey(t, s, dan.ID, []auth.Scope{auth.ScopeJobsCreate, auth.ScopeJobsRead}, 90, time.Now())
	if k.Status != auth.KeyActive {
		t.Errorf("new key status %q, want active", k.Status)
	}
	u, got, stale, err := s.Lookup(ctx, auth.HashToken(secret))
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if u.ID != dan.ID || got.ID != k.ID || got.UserID != dan.ID || !stale || got.Status != auth.KeyActive {
		t.Errorf("Lookup = user %s key %s owner %s stale %v status %s", u.ID, got.ID, got.UserID, stale, got.Status)
	}
	if len(got.Scopes) != 2 || !got.Allows(auth.ScopeJobsRead) || !got.Allows(auth.ScopeJobsCreate) {
		t.Errorf("scopes read back as %v", got.Scopes)
	}

	// Only the hash is stored: no column holds the secret, or any part of
	// it but the 4-character hint.
	var row string
	if err := db.QueryRow(`SELECT api_keys::text FROM api_keys WHERE id = $1`, k.ID).Scan(&row); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains([]byte(row), []byte(secret[4:20])) || bytes.Contains([]byte(row), []byte(secret)) {
		t.Error("the api_keys row holds the secret")
	}

	if _, _, _, err := s.Lookup(ctx, auth.HashToken("gfk_"+secret[4:46]+"A")); !errors.Is(err, auth.ErrKeyNotFound) {
		t.Errorf("unknown key: err %v, want ErrKeyNotFound", err)
	}
}

// Revoked and expired keys fail on the very next lookup: nothing is cached.
func TestAPIKeyStoreRevokedAndExpiredKeysStopAtOnce(t *testing.T) {
	t.Parallel()
	db, dan, _ := newSessionFixture(t)
	s := NewAPIKeyStore(db)
	ctx := context.Background()

	secret, k := newKey(t, s, dan.ID, createOnly, 30, time.Now())
	if _, _, _, err := s.Lookup(ctx, auth.HashToken(secret)); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(ctx, dan.ID, k.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.Lookup(ctx, auth.HashToken(secret)); !errors.Is(err, auth.ErrKeyNotFound) {
		t.Errorf("revoked key: err %v, want ErrKeyNotFound", err)
	}

	// A key that ran out 1 second ago, on PostgreSQL's clock.
	old, k2 := newKey(t, s, dan.ID, createOnly, 30, time.Now())
	if _, err := db.Exec(`UPDATE api_keys SET created_at = now() - interval '31 days', expires_at = now() - interval '1 second' WHERE id = $1`, k2.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.Lookup(ctx, auth.HashToken(old)); !errors.Is(err, auth.ErrKeyNotFound) {
		t.Errorf("expired key: err %v, want ErrKeyNotFound", err)
	}
}

// The list is permanent history, newest first, every key with its status,
// and no hashes.
func TestAPIKeyStoreListShowsEveryKeyWithItsStatus(t *testing.T) {
	t.Parallel()
	db, dan, _ := newSessionFixture(t)
	s := NewAPIKeyStore(db)
	ctx := context.Background()

	base := time.Now().Add(-time.Hour)
	_, expired := newKey(t, s, dan.ID, createOnly, 30, base.AddDate(0, 0, -40))
	_, revoked := newKey(t, s, dan.ID, createOnly, 30, base)
	_, active := newKey(t, s, dan.ID, createOnly, 365, base.Add(time.Minute))
	if err := s.Revoke(ctx, dan.ID, revoked.ID); err != nil {
		t.Fatal(err)
	}

	keys, err := s.List(ctx, dan.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		id, status string
	}{{active.ID, auth.KeyActive}, {revoked.ID, auth.KeyRevoked}, {expired.ID, auth.KeyExpired}}
	if len(keys) != len(want) {
		t.Fatalf("listed %d keys, want %d", len(keys), len(want))
	}
	for i, w := range want {
		if keys[i].ID != w.id || keys[i].Status != w.status {
			t.Errorf("key %d: %s %s, want %s %s", i, keys[i].ID, keys[i].Status, w.id, w.status)
		}
		if keys[i].KeyHash != nil {
			t.Errorf("key %d came back with its hash", i)
		}
	}
	if keys[1].RevokedAt == nil {
		t.Error("the revoked key has no revoked_at")
	}
	if other, err := s.List(ctx, "ffffffffffffffffffffffffffffffff"); err != nil || len(other) != 0 {
		t.Errorf("another user's list: %d keys, %v", len(other), err)
	}
}

// Ownership: a user can't revoke someone else's key, and revoking twice is fine.
func TestAPIKeyStoreRevokeIsForTheOwnerOnly(t *testing.T) {
	t.Parallel()
	db, dan, eve, _ := newTwoUserFixture(t)
	s := NewAPIKeyStore(db)
	ctx := context.Background()

	secret, k := newKey(t, s, dan.ID, createOnly, 90, time.Now())
	if err := s.Revoke(ctx, eve.ID, k.ID); !errors.Is(err, auth.ErrKeyNotFound) {
		t.Errorf("eve revoking dan's key: err %v, want ErrKeyNotFound", err)
	}
	if _, _, _, err := s.Lookup(ctx, auth.HashToken(secret)); err != nil {
		t.Errorf("eve's attempt revoked dan's key: %v", err)
	}
	if err := s.Revoke(ctx, dan.ID, "ffffffffffffffffffffffffffffffff"); !errors.Is(err, auth.ErrKeyNotFound) {
		t.Errorf("unknown key: err %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := s.Revoke(ctx, dan.ID, k.ID); err != nil {
			t.Errorf("revoke %d: %v", i+1, err)
		}
	}

	_, e1 := newKey(t, s, eve.ID, createOnly, 90, time.Now())
	newKey(t, s, dan.ID, createOnly, 90, time.Now())
	newKey(t, s, dan.ID, createOnly, 90, time.Now())
	if n, err := s.RevokeAll(ctx, dan.ID); err != nil || n != 2 {
		t.Errorf("RevokeAll = %d, %v; want dan's 2 unrevoked keys", n, err)
	}
	keys, _ := s.List(ctx, eve.ID)
	if len(keys) != 1 || keys[0].ID != e1.ID || keys[0].Status != auth.KeyActive {
		t.Error("dan's revoke-all touched eve's key")
	}
}

// At most 10 active keys, even when 20 creates arrive at once: the users
// row lock makes them count one after another.
func TestAPIKeyStoreAllowsTenActiveKeysUnderConcurrency(t *testing.T) {
	t.Parallel()
	db, dan, _ := newSessionFixture(t)
	withConns(db, 8)
	expectConnWaits(db)
	s := NewAPIKeyStore(db)
	ctx := context.Background()

	var wg sync.WaitGroup
	var mu sync.Mutex
	made, refused := 0, 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, k, _ := auth.NewAPIKey(dan.ID, "k", createOnly, 30, time.Now())
			err := s.Create(ctx, k)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				made++
			case errors.Is(err, auth.ErrTooManyKeys):
				refused++
			default:
				t.Errorf("create: %v", err)
			}
		}()
	}
	wg.Wait()
	if made != auth.MaxActiveKeys || refused != 10 {
		t.Fatalf("made %d, refused %d; want 10 and 10", made, refused)
	}

	// Revoked and expired keys don't count.
	keys, _ := s.List(ctx, dan.ID)
	if err := s.Revoke(ctx, dan.ID, keys[0].ID); err != nil {
		t.Fatal(err)
	}
	newKey(t, s, dan.ID, createOnly, 30, time.Now())
	_, k, _ := auth.NewAPIKey(dan.ID, "k", createOnly, 30, time.Now())
	if err := s.Create(ctx, k); !errors.Is(err, auth.ErrTooManyKeys) {
		t.Errorf("11th active key: err %v, want ErrTooManyKeys", err)
	}
	if _, err := db.Exec(`UPDATE api_keys SET created_at = now() - interval '31 days', expires_at = now() - interval '1 day' WHERE id = $1`, keys[1].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, k); err != nil {
		t.Errorf("an expired key still counted: %v", err)
	}
}

// last_used_at is written at most once a minute, even when 50 requests
// with the same key arrive together. A trigger in this test's own database
// copy counts the row versions actually written.
func TestAPIKeyTouchWritesOnceUnderConcurrency(t *testing.T) {
	t.Parallel()
	db, dan, _ := newSessionFixture(t)
	withConns(db, 10)
	expectConnWaits(db)
	s := NewAPIKeyStore(db)
	ctx := context.Background()
	if _, err := db.Exec(`
		CREATE TABLE touch_count (n int NOT NULL);
		INSERT INTO touch_count VALUES (0);
		CREATE FUNCTION count_touch() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN UPDATE touch_count SET n = n + 1; RETURN NEW; END $$;
		CREATE TRIGGER count_touch AFTER UPDATE ON api_keys FOR EACH ROW EXECUTE FUNCTION count_touch();`); err != nil {
		t.Fatal(err)
	}

	secret, k := newKey(t, s, dan.ID, createOnly, 90, time.Now())
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// What the API does: look up, and touch if the lookup said stale.
			_, got, stale, err := s.Lookup(ctx, auth.HashToken(secret))
			if err != nil {
				t.Error(err)
				return
			}
			if stale {
				if err := s.Touch(ctx, got.ID); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()

	var n int
	if err := db.QueryRow(`SELECT n FROM touch_count`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("50 concurrent uses wrote last_used_at %d times, want 1", n)
	}
	if _, _, stale, _ := s.Lookup(ctx, auth.HashToken(secret)); stale {
		t.Error("a key used a moment ago still reads as stale")
	}

	// A minute later (moved back on PostgreSQL's clock) one write again.
	if _, err := db.Exec(`UPDATE api_keys SET last_used_at = now() - interval '61 seconds' WHERE id = $1`, k.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Touch(ctx, k.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.QueryRow(`SELECT n FROM touch_count`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 { // 1, the manual move, then exactly one more
		t.Errorf("row written %d times after a minute, want 3 (1 + the test's own update + 1)", n)
	}
}

func TestAPIKeysTableConstraints(t *testing.T) {
	t.Parallel()
	db, dan, _ := newSessionFixture(t)
	insert := func(id, name, hint, scopes string, hashLen int, expires string) error {
		_, err := db.Exec(`INSERT INTO api_keys (id, user_id, name, key_hash, hint, scopes, created_at, expires_at)
			VALUES ($1, $2, $3, substring(sha512(convert_to($8, 'UTF8')) from 1 for $4), $5, $6::text[], now(), now() + $7::interval)`,
			id, dan.ID, name, hashLen, hint, scopes, expires, id)
		return err
	}
	ok := "{jobs:create}"
	if err := insert("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "k", "abcd", ok, 32, "1 day"); err != nil {
		t.Fatalf("a valid row was refused: %v", err)
	}
	cases := map[string]error{
		"bad id":         insert("not-hex", "k", "abcd", ok, 32, "1 day"),
		"empty name":     insert("b0000000000000000000000000000000", "", "abcd", ok, 32, "1 day"),
		"short hint":     insert("b1000000000000000000000000000000", "k", "abc", ok, 32, "1 day"),
		"short hash":     insert("b2000000000000000000000000000000", "k", "abcd", ok, 31, "1 day"),
		"no scopes":      insert("b3000000000000000000000000000000", "k", "abcd", "{}", 32, "1 day"),
		"unknown scope":  insert("b4000000000000000000000000000000", "k", "abcd", "{jobs:delete}", 32, "1 day"),
		"scope twice":    insert("b5000000000000000000000000000000", "k", "abcd", "{jobs:read,jobs:read}", 32, "1 day"),
		"null scope":     insert("b6000000000000000000000000000000", "k", "abcd", "{NULL}", 32, "1 day"),
		"three scopes":   insert("b7000000000000000000000000000000", "k", "abcd", "{jobs:read,jobs:create,jobs:read}", 32, "1 day"),
		"expires before": insert("b8000000000000000000000000000000", "k", "abcd", ok, 32, "-1 day"),
	}
	for name, err := range cases {
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A job remembers which key made it; if the key's row ever goes, the job
// stays and forgets the key (condition 7).
func TestJobsRememberTheirKey(t *testing.T) {
	t.Parallel()
	f := newJobsFixture(t)
	keys := NewAPIKeyStore(f.db)
	ctx := context.Background()
	_, k := newKey(t, keys, f.alice, createOnly, 90, time.Now())

	byKey := mustNewJob(t, f.alice, "made by a key")
	byKey.APIKeyID = k.ID
	byPerson := mustNewJob(t, f.alice, "made by a person")
	for _, j := range []*job.Job{byKey, byPerson} {
		if err := f.s.Add(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	got, err := f.s.Get(ctx, f.alice, byKey.ID)
	if err != nil || got.APIKeyID != k.ID {
		t.Fatalf("job by key: api_key_id %q, %v; want %s", got.APIKeyID, err, k.ID)
	}
	page, err := f.s.List(ctx, f.alice, job.ListParams{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range page.Jobs {
		want := ""
		if j.ID == byKey.ID {
			want = k.ID
		}
		if j.APIKeyID != want {
			t.Errorf("listed %s with api_key_id %q, want %q", j.Title, j.APIKeyID, want)
		}
	}

	if _, err := f.db.Exec(`DELETE FROM api_keys WHERE id = $1`, k.ID); err != nil {
		t.Fatal(err)
	}
	got, err = f.s.Get(ctx, f.alice, byKey.ID)
	if err != nil || got.APIKeyID != "" {
		t.Errorf("after the key's row went: job %v, api_key_id %q; want the job, with no key", err, got.APIKeyID)
	}
}

// An owner reset revokes every key in the same transaction (K4 A); a key
// the reset missed would outlive the "someone else has this account" fix.
func TestUserStoreResetPasswordRevokesKeys(t *testing.T) {
	t.Parallel()
	db, dan, eve, _ := newTwoUserFixture(t)
	keys := NewAPIKeyStore(db)
	users := NewUserStore(db)
	ctx := context.Background()

	s1, _ := newKey(t, keys, dan.ID, createOnly, 90, time.Now())
	s2, _ := newKey(t, keys, dan.ID, []auth.Scope{auth.ScopeJobsRead}, 365, time.Now())
	se, _ := newKey(t, keys, eve.ID, createOnly, 90, time.Now())
	hash, err := testHasher.Hash("a reset password for dan")
	if err != nil {
		t.Fatal(err)
	}
	res, err := users.ResetPassword(ctx, dan.ID, hash)
	if err != nil || res.KeysRevoked != 2 {
		t.Fatalf("ResetPassword = %+v, %v; want 2 keys revoked", res, err)
	}
	for _, s := range []string{s1, s2} {
		if _, _, _, err := keys.Lookup(ctx, auth.HashToken(s)); !errors.Is(err, auth.ErrKeyNotFound) {
			t.Errorf("a key survived the reset: %v", err)
		}
	}
	if _, _, _, err := keys.Lookup(ctx, auth.HashToken(se)); err != nil {
		t.Errorf("dan's reset revoked eve's key: %v", err)
	}

	// All or nothing: with revoking broken, the password stays as it was.
	s3, _ := newKey(t, keys, dan.ID, createOnly, 90, time.Now())
	if _, err := db.Exec(`ALTER TABLE api_keys RENAME COLUMN revoked_at TO revoked_at_gone`); err != nil {
		t.Fatal(err)
	}
	hash2, _ := testHasher.Hash("another password for dan")
	if _, err := users.ResetPassword(ctx, dan.ID, hash2); err == nil {
		t.Fatal("ResetPassword succeeded with a broken api_keys table")
	}
	if _, err := db.Exec(`ALTER TABLE api_keys RENAME COLUMN revoked_at_gone TO revoked_at`); err != nil {
		t.Fatal(err)
	}
	got, _ := users.GetByEmail(ctx, dan.Email)
	if got.PasswordHash != hash {
		t.Error("the failed reset changed the password anyway")
	}
	if _, _, _, err := keys.Lookup(ctx, auth.HashToken(s3)); err != nil {
		t.Errorf("the failed reset revoked a key: %v", err)
	}
}

// Migration 14 on a database that already has jobs: they keep everything
// and get no key.
func TestMigration14KeepsExistingJobs(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	ctx := context.Background()
	upTo13 := fstest.MapFS{}
	matches, err := fs.Glob(migrationFiles, "migrations/00*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range matches {
		if name >= "migrations/0014" {
			continue
		}
		body, err := fs.ReadFile(migrationFiles, name)
		if err != nil {
			t.Fatal(err)
		}
		upTo13[name] = &fstest.MapFile{Data: body}
	}
	if len(upTo13) != 13 {
		t.Fatalf("found %d migrations before 14, want 13", len(upTo13))
	}
	if _, err := migrate(ctx, db, upTo13); err != nil {
		t.Fatalf("migrate to 13: %v", err)
	}
	u := mustNewUser(t, "old@example.com")
	if err := NewUserStore(db).Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := db.Exec(`INSERT INTO jobs (id, user_id, title, status, created_at, updated_at) VALUES ($1, $2, 'old', 'completed', now(), now())`,
			fmt.Sprintf("%032x", i+1), u.ID); err != nil {
			t.Fatal(err)
		}
	}
	applied, err := Migrate(ctx, db)
	if err != nil || len(applied) != 1 || applied[0] != 14 {
		t.Fatalf("Migrate = %v, %v; want [14]", applied, err)
	}
	var n, withKey int
	if err := db.QueryRow(`SELECT count(*), count(api_key_id) FROM jobs`).Scan(&n, &withKey); err != nil {
		t.Fatal(err)
	}
	if n != 3 || withKey != 0 {
		t.Errorf("after migration 14: %d jobs, %d with a key; want 3 and 0", n, withKey)
	}
}
