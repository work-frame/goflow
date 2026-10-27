package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/work-frame/goflow/internal/client"
	"github.com/work-frame/goflow/internal/credentials"
)

// Authenticator is what login and logout need from the API. client.Client satisfies it.
type Authenticator interface {
	Login(ctx context.Context, email, password string) (token string, expiresAt time.Time, err error)
	Logout(ctx context.Context, token string) error
}

// CredentialStore keeps the token between commands. credentials.File satisfies it.
type CredentialStore interface {
	Save(c credentials.Credentials) error
	Load() (credentials.Credentials, error)
	Delete() error
}

// Prompter asks the user for input. A secret answer must not be echoed.
type Prompter interface {
	Prompt(label string, secret bool) (string, error)
}

// Login asks for an email and password, logs in to the API at apiURL and
// saves the token in store. It returns the process exit code. The password
// and the token are never printed.
func Login(ctx context.Context, p Prompter, a Authenticator, store CredentialStore, apiURL string, stdout, stderr io.Writer) int {
	email, err := p.Prompt("Email: ", false)
	if err != nil {
		fmt.Fprintf(stderr, "error: read email: %v\n", err)
		return exitError
	}
	password, err := p.Prompt("Password: ", true)
	if err != nil {
		fmt.Fprintf(stderr, "error: read password: %v\n", err)
		return exitError
	}

	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	token, expiresAt, err := a.Login(ctx, strings.TrimSpace(email), password)
	if errors.Is(err, client.ErrInvalidCredentials) {
		fmt.Fprintln(stderr, "error: invalid email or password")
		return exitError
	}
	if err != nil {
		fmt.Fprintf(stderr, "error: login: %v\n", err)
		return exitError
	}

	err = store.Save(credentials.Credentials{APIURL: apiURL, Token: token, ExpiresAt: expiresAt})
	if err != nil {
		// The server has a session we can't keep. End it, so a token that
		// nobody holds doesn't stay valid for a day.
		_ = a.Logout(ctx, token)
		fmt.Fprintf(stderr, "error: save login: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "logged in to %s until %s\n", apiURL, expiresAt.Local().Format("2006-01-02 15:04"))
	return exitOK
}

// Logout ends the saved session on the server and deletes the local file.
// Deleting the file alone is not enough: a copy of the token would keep
// working until it expires. If the server can't be reached, the file is
// still deleted, and the user is told the session stays valid until expiry.
func Logout(ctx context.Context, a Authenticator, store CredentialStore, stdout, stderr io.Writer) int {
	c, err := store.Load()
	if errors.Is(err, credentials.ErrNotFound) {
		fmt.Fprintln(stdout, "not logged in")
		return exitOK
	}
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitError
	}

	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	serverErr := a.Logout(ctx, c.Token)
	if errors.Is(serverErr, client.ErrUnauthenticated) {
		serverErr = nil // already expired or ended elsewhere: nothing left to end
	}
	if err := store.Delete(); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitError
	}
	if serverErr != nil {
		fmt.Fprintf(stderr, "warning: deleted the local login, but could not end the session on the server (%v).\nIt stays valid until %s.\n",
			serverErr, c.ExpiresAt.Local().Format("2006-01-02 15:04"))
		return exitError
	}
	fmt.Fprintln(stdout, "logged out")
	return exitOK
}
