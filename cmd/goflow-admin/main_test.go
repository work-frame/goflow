package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/work-frame/goflow/internal/auth"
	"github.com/work-frame/goflow/internal/database"
)

// fakePrompter answers in order and fails the test if asked more.
type fakePrompter struct {
	t       *testing.T
	answers []string
}

func (f *fakePrompter) next(label string) (string, error) {
	if len(f.answers) == 0 {
		f.t.Fatalf("asked %q with no answer left", label)
	}
	a := f.answers[0]
	f.answers = f.answers[1:]
	return a, nil
}
func (f *fakePrompter) Secret(label string) (string, error) { return f.next(label) }
func (f *fakePrompter) Line(label string) (string, error)   { return f.next(label) }

func randomSuffix(t *testing.T) string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// fixture is a disposable, migrated database with two accounts: dan, the
// one being reset, with 3 sessions, and eve, with 1, who must be left alone.
type fixture struct {
	url, danEmail, danID, eveID string
	db                          *sql.DB
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := os.Getenv("GOFLOW_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("GOFLOW_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := database.Open(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	name := "goflow_admin_test_" + randomSuffix(t)
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		admin.Close()
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	f := &fixture{url: u.String(), danEmail: "dan@example.com"}
	if f.db, err = database.Open(ctx, f.url); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Close() })
	if _, err := database.Migrate(ctx, f.db); err != nil {
		t.Fatal(err)
	}
	users, sessions := database.NewUserStore(f.db), database.NewSessionStore(f.db)
	for _, who := range []struct {
		email string
		id    *string
		n     int
	}{{f.danEmail, &f.danID, 3}, {"eve@example.com", &f.eveID, 1}} {
		user, err := auth.NewUser(who.email, "the old password, long enough", auth.Hasher{Cost: 4})
		if err != nil {
			t.Fatal(err)
		}
		if err := users.Create(ctx, user); err != nil {
			t.Fatal(err)
		}
		*who.id = user.ID
		for i := 0; i < who.n; i++ {
			_, s := auth.NewSession(user.ID, time.Now())
			if err := sessions.Create(ctx, s); err != nil {
				t.Fatal(err)
			}
		}
	}
	return f
}

func (f *fixture) hash(t *testing.T, id string) string {
	t.Helper()
	var h string
	if err := f.db.QueryRow(`SELECT password_hash FROM users WHERE id = $1`, id).Scan(&h); err != nil {
		t.Fatal(err)
	}
	return h
}

func (f *fixture) sessions(t *testing.T, id string) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(`SELECT count(*) FROM sessions WHERE user_id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func env(cost string) func(string) string {
	return func(k string) string {
		if k == "GOFLOW_BCRYPT_COST" {
			return cost
		}
		return ""
	}
}

const newPassword = "a brand new owner password"

func TestResetChangesThePasswordAndEndsEverySession(t *testing.T) {
	f := newFixture(t)
	var out bytes.Buffer
	p := &fakePrompter{t: t, answers: []string{f.url, " DAN@example.com ", newPassword, newPassword, "reset"}}
	if code := run(context.Background(), []string{"reset-password"}, env("10"), p, database.Open, &out); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	got := out.String()
	for _, want := range []string{"3 open sessions", "password changed for dan@example.com (bcrypt cost 10); 3 sessions ended"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	hash := f.hash(t, f.danID)
	if err := (auth.Hasher{}).Compare(hash, newPassword); err != nil {
		t.Errorf("the new password doesn't log in: %v", err)
	}
	if err := (auth.Hasher{}).Compare(hash, "the old password, long enough"); err == nil {
		t.Error("the old password still logs in")
	}
	if !strings.HasPrefix(hash, "$2a$10$") {
		t.Errorf("hashed at the wrong cost: %.7s", hash)
	}
	if n := f.sessions(t, f.danID); n != 0 {
		t.Errorf("%d of dan's sessions survived the reset, want 0", n)
	}
	if n := f.sessions(t, f.eveID); n != 1 {
		t.Errorf("eve has %d sessions after dan's reset, want her 1", n)
	}
	for _, secret := range []string{newPassword, f.url, hash, "$2a$"} {
		if strings.Contains(got, secret) {
			t.Errorf("the output shows a password, the URL or the hash (%.12s...)", secret)
		}
	}
}

// A refusal at any step leaves the account exactly as it was.
func TestNothingChangesUnlessEveryAnswerIsRight(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answers func(f *fixture) []string
		want    string
	}{
		{"not confirmed", func(f *fixture) []string { return []string{f.url, f.danEmail, newPassword, newPassword, "yes"} }, "not confirmed"},
		{"passwords differ", func(f *fixture) []string { return []string{f.url, f.danEmail, newPassword, newPassword + "!"} }, "differ"},
		{"too short", func(f *fixture) []string { return []string{f.url, f.danEmail, "short"} }, "at least 12 characters"},
		{"unknown email", func(f *fixture) []string { return []string{f.url, "nobody@example.com"} }, "no account"},
		{"not an email", func(f *fixture) []string { return []string{f.url, "not an email"} }, "isn't an email"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			before := f.hash(t, f.danID)
			var out bytes.Buffer
			p := &fakePrompter{t: t, answers: tc.answers(f)}
			if code := run(context.Background(), []string{"reset-password"}, env(""), p, database.Open, &out); code != 1 {
				t.Fatalf("exit %d, want 1: %s", code, out.String())
			}
			if !strings.Contains(out.String(), tc.want) || !strings.Contains(out.String(), "nothing changed") {
				t.Errorf("output lacks %q and \"nothing changed\": %s", tc.want, out.String())
			}
			if f.hash(t, f.danID) != before || f.sessions(t, f.danID) != 3 {
				t.Error("the account changed although the reset was refused")
			}
		})
	}
}

// R1: a remote database needs sslmode=verify-full, checked before any
// connection, and the refusal names the host, never the URL.
func TestRemoteNeedsVerifyFull(t *testing.T) {
	for _, raw := range []string{
		"postgres://neondb_owner:s3cret-pw@ep-x.eu-central-1.aws.neon.tech/neondb?sslmode=require",
		"postgres://neondb_owner:s3cret-pw@ep-x.eu-central-1.aws.neon.tech/neondb?sslmode=verify-ca",
		"postgres://neondb_owner:s3cret-pw@ep-x.eu-central-1.aws.neon.tech/neondb",
		"postgres://goflow:s3cret-pw@postgres:5432/goflow?sslmode=disable",
	} {
		var out bytes.Buffer
		p := &fakePrompter{t: t, answers: []string{raw}}
		spy := func(context.Context, string) (*sql.DB, error) {
			t.Fatal("it connected before checking the URL")
			return nil, errors.New("unreachable")
		}
		if code := run(context.Background(), []string{"reset-password"}, env(""), p, spy, &out); code != 1 {
			t.Errorf("exit %d for a URL without verify-full", code)
		}
		if !strings.Contains(out.String(), "verify-full") || strings.Contains(out.String(), "s3cret-pw") {
			t.Errorf("want a verify-full refusal without the password, got: %s", out.String())
		}
	}
}

// R4: the read-only backup role can't reset, and the tool says why before
// asking for the email.
func TestAReadOnlyLoginIsRefused(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	role := "goflow_admin_ro_" + randomSuffix(t)
	stmts := []string{
		"CREATE ROLE " + role + " LOGIN PASSWORD 'read-only-test-password'",
		"GRANT USAGE ON SCHEMA public TO " + role,
		"GRANT SELECT ON ALL TABLES IN SCHEMA public TO " + role,
	}
	for _, s := range stmts {
		if _, err := f.db.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() {
		f.db.ExecContext(context.Background(), "DROP OWNED BY "+role)
		f.db.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+role)
	})
	u, _ := url.Parse(f.url)
	u.User = url.UserPassword(role, "read-only-test-password")

	before := f.hash(t, f.danID)
	var out bytes.Buffer
	p := &fakePrompter{t: t, answers: []string{u.String()}}
	if code := run(ctx, []string{"reset-password"}, env(""), p, database.Open, &out); code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "may only read") {
		t.Errorf("want the read-only refusal, got: %s", out.String())
	}
	if f.hash(t, f.danID) != before {
		t.Error("the password changed through a read-only login")
	}
}

// R2: the API's cost rule, checked before anything is asked.
func TestBcryptCostFollowsTheAPIsRule(t *testing.T) {
	for _, cost := range []string{"9", "15", "twelve"} {
		var out bytes.Buffer
		p := &fakePrompter{t: t} // no answers: asking anything fails the test
		if code := run(context.Background(), []string{"reset-password"}, env(cost), p, database.Open, &out); code != 1 {
			t.Errorf("GOFLOW_BCRYPT_COST=%s: exit %d, want 1", cost, code)
		}
		if !strings.Contains(out.String(), "GOFLOW_BCRYPT_COST") {
			t.Errorf("GOFLOW_BCRYPT_COST=%s: %s", cost, out.String())
		}
	}
}

func TestNeedsATerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if _, err := newTerminal(r, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Errorf("a pipe was accepted as the terminal: err = %v", err)
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"reset"}, {"reset-password", "dan@example.com"}} {
		var out bytes.Buffer
		if code := run(context.Background(), args, env(""), &fakePrompter{t: t}, database.Open, &out); code != 2 {
			t.Errorf("args %q: exit %d, want 2", args, code)
		}
	}
}
