package job

import (
	"errors"
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	tests := []struct {
		name    string
		title   string
		wantErr error
	}{
		{"valid title", "send welcome email", nil},
		{"trims surrounding whitespace", "  send welcome email  ", nil},
		{"empty title", "", ErrEmptyTitle},
		{"whitespace only", "   \t\n", ErrEmptyTitle},
		{"exactly 200 characters", strings.Repeat("a", 200), nil},
		{"201 characters", strings.Repeat("a", 201), ErrTitleTooLong},
		{"200 multi-byte characters", strings.Repeat("é", 200), nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			j, err := New(tc.title)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				if j != nil {
					t.Errorf("expected nil job on error, got %+v", j)
				}
				return
			}

			if j.Title != strings.TrimSpace(tc.title) {
				t.Errorf("Title = %q, want %q", j.Title, strings.TrimSpace(tc.title))
			}
			if j.Status != StatusPending {
				t.Errorf("Status = %q, want %q", j.Status, StatusPending)
			}
			if len(j.ID) != 32 {
				t.Errorf("len(ID) = %d, want 32", len(j.ID))
			}
			if j.CreatedAt.IsZero() {
				t.Error("CreatedAt is zero")
			}
			if !j.CreatedAt.Equal(j.UpdatedAt) {
				t.Errorf("CreatedAt %v != UpdatedAt %v", j.CreatedAt, j.UpdatedAt)
			}
		})
	}
}

func TestNewGeneratesUniqueIDs(t *testing.T) {
	const n = 10000
	seen := make(map[string]bool, n)

	for i := 0; i < n; i++ {
		j, err := New("job")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if seen[j.ID] {
			t.Fatalf("duplicate ID after %d jobs: %s", i, j.ID)
		}
		seen[j.ID] = true
	}
}

func TestStatusValid(t *testing.T) {
	valid := []Status{StatusPending, StatusQueued, StatusProcessing, StatusCompleted, StatusFailed}
	for _, s := range valid {
		if !s.Valid() {
			t.Errorf("%q should be valid", s)
		}
	}

	invalid := []Status{"", "PENDING", "done", "banana"}
	for _, s := range invalid {
		if s.Valid() {
			t.Errorf("%q should be invalid", s)
		}
	}
}
