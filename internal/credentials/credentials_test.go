package credentials

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const api = "http://localhost:8080"

func newFile(t *testing.T) File {
	t.Helper()
	return File{Path: filepath.Join(t.TempDir(), "config", "goflow", "credentials")}
}

func sample() Credentials {
	return Credentials{
		APIURL:    api,
		Token:     "gfs_" + strings.Repeat("A", 43),
		ExpiresAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestSaveAndLoad(t *testing.T) {
	f := newFile(t)
	if err := f.Save(sample()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := f.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != sample() {
		t.Errorf("Load = %+v, want %+v", got, sample())
	}
}

// The token file must be private from the start: 0600, in a 0700 directory.
func TestSaveIsPrivate(t *testing.T) {
	f := newFile(t)
	if err := f.Save(sample()); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %04o, want 0600", perm)
	}
	dir, err := os.Stat(filepath.Dir(f.Path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dir.Mode().Perm(); perm != 0o700 {
		t.Errorf("directory mode = %04o, want 0700", perm)
	}
}

func TestSaveReplacesAndLeavesNoTempFiles(t *testing.T) {
	f := newFile(t)
	first := sample()
	second := sample()
	second.Token = "gfs_" + strings.Repeat("B", 43)

	if err := f.Save(first); err != nil {
		t.Fatal(err)
	}
	if err := f.Save(second); err != nil {
		t.Fatal(err)
	}
	got, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != second.Token {
		t.Error("second Save did not replace the first")
	}
	entries, err := os.ReadDir(filepath.Dir(f.Path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory has %d entries, want only the credentials file", len(entries))
	}
}

func TestLoadMissing(t *testing.T) {
	if _, err := newFile(t).Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// Like ssh with a private key: a file others can read is refused.
func TestLoadRefusesAFileOthersCanRead(t *testing.T) {
	f := newFile(t)
	if err := f.Save(sample()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.Path, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := f.Load()
	if !errors.Is(err, ErrInsecure) {
		t.Fatalf("err = %v, want ErrInsecure", err)
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("error doesn't say how to fix it: %v", err)
	}
	if strings.Contains(err.Error(), sample().Token) {
		t.Error("error contains the token")
	}
}

func TestDelete(t *testing.T) {
	f := newFile(t)
	if err := f.Save(sample()); err != nil {
		t.Fatal(err)
	}
	if err := f.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := f.Load(); !errors.Is(err, ErrNotFound) {
		t.Errorf("after Delete: err = %v, want ErrNotFound", err)
	}
	if err := f.Delete(); err != nil {
		t.Errorf("second Delete: %v, want nil", err)
	}
}

func TestTokenFor(t *testing.T) {
	f := newFile(t)
	before := sample().ExpiresAt.Add(-time.Hour)

	if tok, err := f.TokenFor(api, before); err != nil || tok != "" {
		t.Errorf("not logged in: got %q, %v; want \"\", nil", tok, err)
	}
	if err := f.Save(sample()); err != nil {
		t.Fatal(err)
	}
	if tok, _ := f.TokenFor(api, before); tok != sample().Token {
		t.Errorf("same API, not expired: got %q, want the token", tok)
	}
	if tok, _ := f.TokenFor("https://evil.example.com", before); tok != "" {
		t.Error("the token was offered to a different API")
	}
	if tok, _ := f.TokenFor(api, sample().ExpiresAt); tok != "" {
		t.Error("an expired token was offered")
	}

	if err := os.Chmod(f.Path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := f.TokenFor(api, before); !errors.Is(err, ErrInsecure) {
		t.Errorf("insecure file: err = %v, want ErrInsecure", err)
	}
}
