package job

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

var ErrInvalidStatus = errors.New("job: invalid status")

// Load reads jobs from the JSON file at path.
// A missing file is not an error: it returns an empty store.
func Load(path string) (*MemoryStore, error) {
	s := NewMemoryStore()

	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("job: read %s: %w", path, err)
	}

	var jobs []*Job
	if err := json.Unmarshal(data, &jobs); err != nil {
		return nil, fmt.Errorf("job: parse %s: %w", path, err)
	}

	for _, j := range jobs {
		if j == nil {
			return nil, fmt.Errorf("job: load %s: %w", path, ErrNilJob)
		}
		if !j.Status.Valid() {
			return nil, fmt.Errorf("job: load %s: job %s: %w %q", path, j.ID, ErrInvalidStatus, j.Status)
		}
		if err := s.Add(j); err != nil {
			return nil, fmt.Errorf("job: load %s: job %s: %w", path, j.ID, err)
		}
	}
	return s, nil
}

// Save writes all jobs to path as JSON.
// It writes a temporary file first, then renames it over path,
// so a crash mid-write never leaves a half-written file behind.
func (s *MemoryStore) Save(path string) error {
	data, err := json.MarshalIndent(s.List(), "", "  ")
	if err != nil {
		return fmt.Errorf("job: encode: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("job: create dir %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".jobs-*.tmp")
	if err != nil {
		return fmt.Errorf("job: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // cleans up on failure; harmless after a successful rename

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("job: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("job: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("job: close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("job: replace %s: %w", path, err)
	}
	return nil
}
