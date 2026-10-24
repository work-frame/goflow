package job

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxTitleLength is the maximum number of characters allowed in a job title.
const MaxTitleLength = 200

// Status is the lifecycle state of a job.
type Status string

const (
	StatusPending    Status = "pending"
	StatusQueued     Status = "queued"
	StatusProcessing Status = "processing"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
)

// Valid reports whether s is one of the known statuses.
func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusQueued, StatusProcessing, StatusCompleted, StatusFailed:
		return true
	default:
		return false
	}
}

// Sentinel errors returned by New. Callers check them with errors.Is.
var (
	ErrEmptyTitle   = errors.New("job: title is required")
	ErrTitleTooLong = errors.New("job: title exceeds 200 characters")
)

// Job is a unit of background work.
type Job struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Status    Status    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// New validates the title and returns a pending job.
func New(title string) (*Job, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, ErrEmptyTitle
	}
	if utf8.RuneCountInString(title) > MaxTitleLength {
		return nil, ErrTitleTooLong
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	return &Job{
		ID:        newID(),
		Title:     title,
		Status:    StatusPending,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// newID returns 16 random bytes encoded as a 32-character hex string.
func newID() string {
	b := make([]byte, 16)
	// Since Go 1.24, crypto/rand.Read never returns an error.
	// If the OS random source fails, the program stops instead.
	rand.Read(b)
	return hex.EncodeToString(b)
}
