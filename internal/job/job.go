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
	// StatusRetrying: failed, waiting for its next attempt (NextAttemptAt).
	StatusRetrying  Status = "retrying"
	StatusCompleted Status = "completed"
	// StatusFailed is kept for jobs that failed before retries existed.
	// New failures become StatusRetrying or StatusDead.
	StatusFailed Status = "failed"
	// StatusDead: a permanent error, or every attempt used up.
	StatusDead Status = "dead"
)

// DefaultMaxAttempts is how many times a job runs before it is dead.
const DefaultMaxAttempts = 3

// MaxIdempotencyKeyLength matches the CHECK on jobs.idempotency_key.
const MaxIdempotencyKeyLength = 255

// Valid reports whether s is one of the known statuses.
func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusQueued, StatusProcessing, StatusRetrying, StatusCompleted, StatusFailed, StatusDead:
		return true
	default:
		return false
	}
}

// Rerunnable reports whether a job in status s may be re-run by its owner:
// only one that has stopped for good after failing. A completed job is
// finished work; anything else is still on its way.
func (s Status) Rerunnable() bool {
	return s == StatusDead || s == StatusFailed
}

// Sentinel errors returned by New. Callers check them with errors.Is.
var (
	ErrEmptyTitle   = errors.New("job: title is required")
	ErrTitleTooLong = errors.New("job: title exceeds 200 characters")
)

// ErrBadIdempotencyKey is returned by ValidateIdempotencyKey.
var ErrBadIdempotencyKey = errors.New("job: idempotency key must be 1 to 255 visible ASCII characters")

// ValidateIdempotencyKey accepts 1 to 255 visible ASCII characters (no
// spaces, no control characters). A UUID fits; so does any random token.
// Keeping the alphabet small means a key can't smuggle a newline into a log
// line or look like a different key after Unicode normalisation.
func ValidateIdempotencyKey(key string) error {
	if key == "" || len(key) > MaxIdempotencyKeyLength {
		return ErrBadIdempotencyKey
	}
	for i := 0; i < len(key); i++ {
		if key[i] < '!' || key[i] > '~' {
			return ErrBadIdempotencyKey
		}
	}
	return nil
}

// Job is a unit of background work.
type Job struct {
	ID        string    `json:"id"`
	UserID    string    `json:"-"` // the owner; clients only ever see their own jobs
	Title     string    `json:"title"`
	Status    Status    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Attempts counts the runs so far; MaxAttempts is the limit.
	Attempts    int `json:"attempts"`
	MaxAttempts int `json:"max_attempts"`
	// NextAttemptAt is set while the job is retrying.
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`

	// IdempotencyKey is the client's Idempotency-Key header, or "" if it
	// sent none. Never returned: the client already knows it.
	IdempotencyKey string `json:"-"`
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
		ID:          newID(),
		Title:       title,
		Status:      StatusPending,
		CreatedAt:   now,
		UpdatedAt:   now,
		MaxAttempts: DefaultMaxAttempts,
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

// ErrNoOwner is returned when a job is stored without an owner.
var ErrNoOwner = errors.New("job: no owner")
