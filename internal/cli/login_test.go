package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/work-frame/goflow/internal/client"
	"github.com/work-frame/goflow/internal/credentials"
)

const (
	testAPI      = "http://localhost:8080"
	testEmail    = "dan@example.com"
	testPassword = "correct horse battery"
	testToken    = "gfs_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"
)

// fakePrompter answers prompts in order and remembers which were secret.
type fakePrompter struct {
	answers []string
	secret  []bool
}

func (p *fakePrompter) Prompt(_ string, secret bool) (string, error) {
	p.secret = append(p.secret, secret)
	if len(p.answers) == 0 {
		return "", errors.New("no more answers")
	}
	a := p.answers[0]
	p.answers = p.answers[1:]
	return a, nil
}

// fakeAuth accepts one email and password and records logouts.
type fakeAuth struct {
	logoutErr error
	loggedOut []string
}

func (a *fakeAuth) Login(_ context.Context, email, password string) (string, time.Time, error) {
	if email != testEmail || password != testPassword {
		return "", time.Time{}, client.ErrInvalidCredentials
	}
	return testToken, time.Now().Add(24 * time.Hour).UTC(), nil
}

func (a *fakeAuth) Logout(_ context.Context, token string) error {
	a.loggedOut = append(a.loggedOut, token)
	return a.logoutErr
}

// brokenStore can't save, like a full disk or a read-only home directory.
type brokenStore struct{ credentials.File }

func (brokenStore) Save(credentials.Credentials) error { return errors.New("disk full") }

func newStore(t *testing.T) credentials.File {
	t.Helper()
	return credentials.File{Path: filepath.Join(t.TempDir(), "goflow", "credentials")}
}

func login(t *testing.T, p Prompter, a Authenticator, store CredentialStore) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Login(context.Background(), p, a, store, testAPI, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestLogin(t *testing.T) {
	p := &fakePrompter{answers: []string{"  " + testEmail + " ", testPassword}}
	store := newStore(t)

	code, out, errOut := login(t, p, &fakeAuth{}, store)
	if code != exitOK {
		t.Fatalf("exit code %d, stderr %s", code, errOut)
	}
	if len(p.secret) != 2 || p.secret[0] || !p.secret[1] {
		t.Errorf("secret flags = %v, want the email visible and the password hidden", p.secret)
	}
	saved, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if saved.Token != testToken || saved.APIURL != testAPI {
		t.Errorf("saved %+v, want the token for %s", saved, testAPI)
	}
	for _, secret := range []string{testPassword, testToken} {
		if strings.Contains(out+errOut, secret) {
			t.Errorf("output contains a secret %q", secret)
		}
	}
}

func TestLoginWithWrongPassword(t *testing.T) {
	store := newStore(t)
	p := &fakePrompter{answers: []string{testEmail, "wrong horse battery"}}

	code, _, errOut := login(t, p, &fakeAuth{}, store)
	if code != exitError || !strings.Contains(errOut, "invalid email or password") {
		t.Fatalf("exit %d, stderr %q; want 1 and the invalid-credentials message", code, errOut)
	}
	if _, err := store.Load(); !errors.Is(err, credentials.ErrNotFound) {
		t.Errorf("a failed login saved something: %v", err)
	}
}

// If the token can't be saved, the session it belongs to is ended at once,
// so no valid token is left behind that nobody holds.
func TestLoginEndsTheSessionIfItCannotBeSaved(t *testing.T) {
	a := &fakeAuth{}
	p := &fakePrompter{answers: []string{testEmail, testPassword}}

	code, _, errOut := login(t, p, a, brokenStore{newStore(t)})
	if code != exitError || !strings.Contains(errOut, "disk full") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if len(a.loggedOut) != 1 || a.loggedOut[0] != testToken {
		t.Errorf("server logouts = %v, want the new token ended", a.loggedOut)
	}
}

func logout(t *testing.T, a Authenticator, store CredentialStore) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Logout(context.Background(), a, store, &out, &errOut)
	return code, out.String(), errOut.String()
}

func saveTestLogin(t *testing.T, store credentials.File) {
	t.Helper()
	err := store.Save(credentials.Credentials{APIURL: testAPI, Token: testToken, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLogoutEndsTheServerSessionAndDeletesTheFile(t *testing.T) {
	store := newStore(t)
	saveTestLogin(t, store)
	a := &fakeAuth{}

	code, out, _ := logout(t, a, store)
	if code != exitOK || !strings.Contains(out, "logged out") {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	if len(a.loggedOut) != 1 || a.loggedOut[0] != testToken {
		t.Errorf("server logouts = %v, want the saved token", a.loggedOut)
	}
	if _, err := store.Load(); !errors.Is(err, credentials.ErrNotFound) {
		t.Errorf("credentials file still there: %v", err)
	}
}

// Unreachable server: the local file goes anyway, and the user is told the
// session is still alive on the server.
func TestLogoutWhenTheServerIsDown(t *testing.T) {
	store := newStore(t)
	saveTestLogin(t, store)

	code, _, errOut := logout(t, &fakeAuth{logoutErr: errors.New("connection refused")}, store)
	if code != exitError || !strings.Contains(errOut, "stays valid until") {
		t.Fatalf("exit %d, stderr %q; want 1 and a warning", code, errOut)
	}
	if _, err := store.Load(); !errors.Is(err, credentials.ErrNotFound) {
		t.Errorf("credentials file still there: %v", err)
	}
}

func TestLogoutWhenAlreadyExpired(t *testing.T) {
	store := newStore(t)
	saveTestLogin(t, store)

	code, out, _ := logout(t, &fakeAuth{logoutErr: client.ErrUnauthenticated}, store)
	if code != exitOK || !strings.Contains(out, "logged out") {
		t.Errorf("exit %d, stdout %q; an expired session is already logged out", code, out)
	}
}

func TestLogoutWhenNotLoggedIn(t *testing.T) {
	a := &fakeAuth{}
	code, out, _ := logout(t, a, newStore(t))
	if code != exitOK || !strings.Contains(out, "not logged in") || len(a.loggedOut) != 0 {
		t.Errorf("exit %d, stdout %q, logouts %v", code, out, a.loggedOut)
	}
}

func TestUnauthenticatedCommandsSuggestLogin(t *testing.T) {
	code, _, errOut := run(t, errJobs{err: client.ErrUnauthenticated}, "job", "list")
	if code != exitError || !strings.Contains(errOut, "goflow login") {
		t.Errorf("exit %d, stderr %q; want a hint to run goflow login", code, errOut)
	}
}
