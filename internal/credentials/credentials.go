// Package credentials keeps the CLI's session token in a private file,
// ~/.config/goflow/credentials, readable only by its owner.
package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

var (
	// ErrNotFound means there is no credentials file: nobody has logged in.
	ErrNotFound = errors.New("credentials: not logged in")
	// ErrInsecure means the file can be read or written by other users.
	ErrInsecure = errors.New("credentials: file is accessible to other users")
)

// Credentials is one login. APIURL records which server issued the token, so
// it is never sent to a different one.
type Credentials struct {
	APIURL    string    `json:"api_url"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// File stores Credentials as JSON at Path.
type File struct {
	Path string
}

// DefaultPath is $XDG_CONFIG_HOME/goflow/credentials, which is usually
// ~/.config/goflow/credentials. It is outside every project folder, so it
// can't be committed to git by accident.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("credentials: find config directory: %w", err)
	}
	return filepath.Join(dir, "goflow", "credentials"), nil
}

// Save writes c so that only the current user can read it. The file is
// created with mode 0600 from the first byte (os.CreateTemp does that), then
// renamed into place, so there is never a moment when it is readable by others
// or half-written.
func (f File) Save(c Credentials) error {
	dir := filepath.Dir(f.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("credentials: create %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("credentials: encode: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".credentials-*")
	if err != nil {
		return fmt.Errorf("credentials: create temp file: %w", err)
	}
	defer os.Remove(tmp.Name()) // fails harmlessly once the rename has happened

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("credentials: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("credentials: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("credentials: close: %w", err)
	}
	if err := os.Rename(tmp.Name(), f.Path); err != nil {
		return fmt.Errorf("credentials: save %s: %w", f.Path, err)
	}
	return nil
}

// Load reads the credentials. Like ssh with a private key, it refuses a file
// that other users could read: a token that may already have leaked is not
// used. It returns ErrNotFound if nobody has logged in.
func (f File) Load() (Credentials, error) {
	info, err := os.Stat(f.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return Credentials{}, ErrNotFound
	}
	if err != nil {
		return Credentials{}, fmt.Errorf("credentials: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return Credentials{}, fmt.Errorf("%w: %s has mode %04o; run: chmod 600 %s",
			ErrInsecure, f.Path, info.Mode().Perm(), f.Path)
	}

	data, err := os.ReadFile(f.Path)
	if err != nil {
		return Credentials{}, fmt.Errorf("credentials: %w", err)
	}
	var c Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return Credentials{}, fmt.Errorf("credentials: %s is not valid: %w", f.Path, err)
	}
	return c, nil
}

// Delete removes the file. Deleting a file that is already gone is fine.
func (f File) Delete() error {
	if err := os.Remove(f.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("credentials: %w", err)
	}
	return nil
}

// TokenFor returns the saved token if it was issued by apiURL and hasn't
// expired at now, and "" otherwise. Errors are only for files that exist but
// can't be trusted or read.
func (f File) TokenFor(apiURL string, now time.Time) (string, error) {
	c, err := f.Load()
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if c.APIURL != apiURL || !now.Before(c.ExpiresAt) {
		return "", nil
	}
	return c.Token, nil
}
