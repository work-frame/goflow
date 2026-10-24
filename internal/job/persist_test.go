package job

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")

	original := NewMemoryStore()
	for _, title := range []string{"send email", "resize image"} {
		if err := original.Add(mustNew(t, title)); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	if err := original.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := original.List()
	got := loaded.List()
	if len(got) != len(want) {
		t.Fatalf("loaded %d jobs, want %d", len(got), len(want))
	}
	for i := range want {
		w, g := want[i], got[i]
		if g.ID != w.ID || g.Title != w.Title || g.Status != w.Status {
			t.Errorf("job %d = %+v, want %+v", i, g, w)
		}
		if !g.CreatedAt.Equal(w.CreatedAt) || !g.UpdatedAt.Equal(w.UpdatedAt) {
			t.Errorf("job %d timestamps changed after round trip", i)
		}
	}
}

func TestLoadMissingFileReturnsEmptyStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if n := len(s.List()); n != 0 {
		t.Errorf("got %d jobs, want 0", n)
	}
}

func TestLoadRejectsCorruptJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	writeFile(t, path, "{not json")

	if _, err := Load(path); err == nil {
		t.Error("Load succeeded on corrupt JSON, want error")
	}
}

func TestLoadRejectsInvalidStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	writeFile(t, path, `[{"id":"abc","title":"x","status":"banana",
		"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]`)

	_, err := Load(path)
	if !errors.Is(err, ErrInvalidStatus) {
		t.Errorf("error = %v, want ErrInvalidStatus", err)
	}
}

func TestLoadRejectsDuplicateIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	entry := `{"id":"abc","title":"x","status":"pending",
		"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`
	writeFile(t, path, "["+entry+","+entry+"]")

	_, err := Load(path)
	if !errors.Is(err, ErrDuplicateID) {
		t.Errorf("error = %v, want ErrDuplicateID", err)
	}
}

func TestSaveCreatesMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "jobs.json")

	if err := NewMemoryStore().Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

func TestSaveEmptyStoreWritesEmptyArray(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")

	if err := NewMemoryStore().Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "[]" {
		t.Errorf("file contents = %q, want %q", got, "[]")
	}
}
