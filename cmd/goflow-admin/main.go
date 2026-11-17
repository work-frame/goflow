// Command goflow-admin holds the jobs only the owner of a GoFlow
// deployment does, from their own machine, straight against the database.
// It is not in the container image: nothing on Render can run it.
//
//	go run ./cmd/goflow-admin reset-password
//
// reset-password sets a new password for one account and ends every
// session it has, in one transaction (Task 78). It asks for everything at
// prompts, so nothing lands in shell history: the database URL (hidden),
// the account's email (hidden), the new password twice (hidden), then
// "reset" to confirm. It uses the API's own rules: auth.ValidatePassword
// and the bcrypt cost from GOFLOW_BCRYPT_COST (default 12, 10 to 14).
//
// It never prints the URL, the password or the hash. A remote database
// must be reached with sslmode=verify-full, and the login must be able to
// write (the owner role, not the read-only backup role).
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/work-frame/goflow/internal/auth"
	"github.com/work-frame/goflow/internal/database"
	"github.com/work-frame/goflow/internal/urlcheck"
)

const usage = `usage: go run ./cmd/goflow-admin reset-password

Sets a new password for one account and ends all of its sessions.
Everything is asked for at prompts; nothing goes on the command line.`

// prompter asks the person at the terminal. Secret answers are not shown
// while typed; empty answers to a secret are skipped, so a paste that
// starts with a line break still works.
type prompter interface {
	Secret(label string) (string, error)
	Line(label string) (string, error)
}

// opener connects to the database: database.Open, or a test's spy.
type opener func(ctx context.Context, url string) (*sql.DB, error)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	p, err := newTerminal(os.Stdin, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "stop:", err)
		os.Exit(1)
	}
	os.Exit(run(ctx, os.Args[1:], os.Getenv, p, database.Open, os.Stdout))
}

// run returns the exit status: 0 done, 1 refused or failed, 2 bad usage.
func run(ctx context.Context, args []string, getenv func(string) string, p prompter, open opener, out io.Writer) int {
	if len(args) != 1 || args[0] != "reset-password" {
		fmt.Fprintln(out, usage)
		return 2
	}
	if err := resetPassword(ctx, getenv, p, open, out); err != nil {
		fmt.Fprintln(out, "stop:", err)
		return 1
	}
	return 0
}

func resetPassword(ctx context.Context, getenv func(string) string, p prompter, open opener, out io.Writer) error {
	cost, err := auth.CostSetting(getenv("GOFLOW_BCRYPT_COST"))
	if err != nil {
		return err
	}

	rawURL, err := p.Secret("Database URL (the owner role; hidden): ")
	if err != nil {
		return err
	}
	rawURL = strings.TrimSpace(rawURL)
	if err := urlcheck.DatabaseVerifyFull(rawURL); err != nil {
		return err
	}
	host := hostOf(rawURL)

	dbCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	db, err := open(dbCtx, rawURL)
	if err != nil {
		// The driver's message can quote the connection string; say only
		// which host it was.
		return fmt.Errorf("can't connect to %s: check the URL, your network, and that the database is awake", host)
	}
	defer db.Close()

	var writable bool
	if err := db.QueryRowContext(dbCtx, `SELECT has_table_privilege('users', 'UPDATE')
		AND has_table_privilege('sessions', 'DELETE')`).Scan(&writable); err != nil {
		return fmt.Errorf("can't check what this login may do on %s: %w", host, err)
	}
	if !writable {
		return errors.New("this login can't change passwords (it may only read): use the database owner's URL, not goflow_backup's")
	}

	rawEmail, err := p.Secret("Email of the account (hidden): ")
	if err != nil {
		return err
	}
	email, err := auth.NormalizeEmail(rawEmail)
	if err != nil {
		return errors.New("that isn't an email address; nothing changed")
	}

	type account struct {
		id       string
		created  time.Time
		sessions int
	}
	rows, err := db.QueryContext(dbCtx, `SELECT u.id, u.created_at,
		(SELECT count(*) FROM sessions s WHERE s.user_id = u.id)
		FROM users u WHERE u.email = $1`, email)
	if err != nil {
		return fmt.Errorf("can't look up the account: %w", err)
	}
	var found []account
	for rows.Next() {
		var a account
		if err := rows.Scan(&a.id, &a.created, &a.sessions); err != nil {
			rows.Close()
			return fmt.Errorf("can't look up the account: %w", err)
		}
		found = append(found, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("can't look up the account: %w", err)
	}
	switch len(found) {
	case 1:
	case 0:
		return fmt.Errorf("no account on %s has that email; nothing changed", host)
	default:
		return fmt.Errorf("%d accounts on %s have that email, so there's no telling which one; nothing changed", len(found), host)
	}
	acct := found[0]
	fmt.Fprintf(out, "account %s on %s: created %s, %d open sessions\n",
		email, host, acct.created.UTC().Format("2006-01-02 15:04 UTC"), acct.sessions)

	password, err := p.Secret("New password (hidden, at least 12 characters): ")
	if err != nil {
		return err
	}
	if err := auth.ValidatePassword(password); err != nil {
		return fmt.Errorf("%s; nothing changed", strings.TrimPrefix(err.Error(), "auth: "))
	}
	again, err := p.Secret("The same password again (hidden): ")
	if err != nil {
		return err
	}
	if again != password {
		return errors.New("the two passwords differ; nothing changed")
	}
	confirm, err := p.Line(fmt.Sprintf("Type reset to change the password and end those %d sessions: ", acct.sessions))
	if err != nil {
		return err
	}
	if strings.TrimSpace(confirm) != "reset" {
		return errors.New("not confirmed; nothing changed")
	}

	hash, err := auth.Hasher{Cost: cost}.Hash(password)
	if err != nil {
		return fmt.Errorf("can't hash the password: %w", err)
	}
	ended, err := database.NewUserStore(db).ResetPassword(dbCtx, acct.id, hash)
	if errors.Is(err, auth.ErrUserNotFound) {
		return errors.New("the account was deleted while you typed; nothing changed")
	}
	if err != nil {
		return fmt.Errorf("the reset failed and was rolled back, nothing changed: %w", err)
	}
	fmt.Fprintf(out, "password changed for %s (bcrypt cost %d); %d sessions ended. Log in again everywhere.\n",
		email, cost, ended)
	return nil
}

// hostOf names the database in messages: the host, never the URL, which
// holds the password.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "the database"
	}
	return u.Hostname()
}

// terminal is the real prompter. It refuses to work without a terminal, so
// answers can't come from a file or a pipe that might be kept somewhere.
type terminal struct {
	in  *os.File
	out io.Writer
}

func newTerminal(in *os.File, out io.Writer) (*terminal, error) {
	if !term.IsTerminal(int(in.Fd())) {
		return nil, errors.New("goflow-admin must be run in a terminal: it reads the URL and the password at hidden prompts, never from a pipe or a file")
	}
	return &terminal{in: in, out: out}, nil
}

func (t *terminal) Secret(label string) (string, error) {
	for {
		fmt.Fprint(t.out, label)
		b, err := term.ReadPassword(int(t.in.Fd()))
		fmt.Fprintln(t.out)
		if err != nil {
			return "", fmt.Errorf("read answer: %w", err)
		}
		// Returned as typed: a password's spaces are part of it, exactly as
		// the dashboard sends them. Only an empty answer is skipped.
		if strings.TrimSpace(string(b)) != "" {
			return string(b), nil
		}
	}
}

// Line reads one visible line a byte at a time, so nothing typed after it
// sits in a buffer that a later hidden prompt would miss.
func (t *terminal) Line(label string) (string, error) {
	fmt.Fprint(t.out, label)
	var sb strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := t.in.Read(buf)
		if n == 1 {
			if buf[0] == '\n' {
				return sb.String(), nil
			}
			sb.WriteByte(buf[0])
		}
		if err != nil {
			return "", fmt.Errorf("read answer: %w", err)
		}
	}
}
