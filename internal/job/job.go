package job

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/work-frame/goflow/internal/egress"
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

// MaxMaxAttempts matches the CHECK on jobs.max_attempts: a client may ask
// for 1 to 10 attempts.
const MaxMaxAttempts = 10

// Type says what a job does: which handler runs it and what its payload
// holds.
type Type string

const (
	// TypeDemo jobs have no payload; the title picks what worker.Demo does.
	TypeDemo Type = "demo"
	// TypeWebhook jobs POST a JSON body to an HTTPS URL (Phase 15).
	TypeWebhook Type = "webhook"
)

// Limits on a webhook job's payload. The URL limit is the de facto limit of
// browsers and most servers; the body limit keeps one job from filling a
// worker's memory or the database.
const (
	MaxWebhookURLLength = 2048
	MaxWebhookBodyBytes = 64 << 10
)

// Webhook is a webhook job's payload: where to POST, and what. Body is only
// filled in where it's needed (one job's details, the worker); lists and
// live events leave it out (WithoutBody).
type Webhook struct {
	URL  string          `json:"url"`
	Body json.RawMessage `json:"body,omitempty"`
}

// InvalidError is a validation failure whose message can be shown to the
// client as it is. It never repeats the URL or the body: a URL often holds
// a token in its path or query.
type InvalidError struct{ msg string }

func (e *InvalidError) Error() string { return e.msg }

// Invalidf returns an *InvalidError with a formatted message.
func Invalidf(format string, a ...any) error {
	return &InvalidError{fmt.Sprintf(format, a...)}
}

// ValidateWebhook checks what can be checked without the network: an
// absolute https URL on port 443, with no username, password or fragment,
// and a JSON body of at most MaxWebhookBodyBytes. Whether the host's
// addresses may be reached is decided when connecting, every time, because
// a DNS answer can change after this check.
func ValidateWebhook(w Webhook) error {
	if len(w.URL) > MaxWebhookURLLength {
		return Invalidf("webhook.url must be at most %d bytes", MaxWebhookURLLength)
	}
	u, err := url.Parse(w.URL)
	if err != nil || u.Opaque != "" || u.Hostname() == "" {
		return Invalidf("webhook.url must be an absolute https:// URL")
	}
	if u.Scheme != "https" {
		return Invalidf("webhook.url must use https")
	}
	if u.User != nil {
		return Invalidf("webhook.url must not contain a username or password")
	}
	if p := u.Port(); p != "" && p != "443" {
		return Invalidf("webhook.url must use port 443")
	}
	if u.Fragment != "" || strings.Contains(w.URL, "#") {
		return Invalidf("webhook.url must not contain a #fragment")
	}
	if !publicHost(u.Hostname()) {
		return Invalidf("webhook.url points at a private, loopback or reserved address")
	}
	if len(w.Body) == 0 {
		return Invalidf("webhook.body is required")
	}
	if len(w.Body) > MaxWebhookBodyBytes {
		return Invalidf("webhook.body must be at most %d bytes", MaxWebhookBodyBytes)
	}
	if !json.Valid(w.Body) {
		return Invalidf("webhook.body must be JSON")
	}
	return nil
}

// publicHost refuses the hosts that can be refused without DNS: an IP
// address that egress.Allowed rejects, and localhost names. A name that
// resolves to a private address passes here and is refused when the worker
// connects (internal/egress), the check that counts.
func publicHost(host string) bool {
	if ip, err := netip.ParseAddr(host); err == nil {
		return egress.Allowed(ip)
	}
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	return h != "localhost" && !strings.HasSuffix(h, ".localhost")
}

// ValidateMaxAttempts accepts 1 to MaxMaxAttempts.
func ValidateMaxAttempts(n int) error {
	if n < 1 || n > MaxMaxAttempts {
		return Invalidf("max_attempts must be between 1 and %d", MaxMaxAttempts)
	}
	return nil
}

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
	Type      Type      `json:"type"`
	Status    Status    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Attempts counts the runs so far; MaxAttempts is the limit.
	Attempts    int `json:"attempts"`
	MaxAttempts int `json:"max_attempts"`
	// NextAttemptAt is set while the job is retrying.
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`

	// Version starts at 1 and goes up by 1 with every change, in the order
	// the changes were committed. Of two copies of a job, the one with the
	// higher version is newer; updated_at can't promise that.
	Version int64 `json:"version"`

	// IdempotencyKey is the client's Idempotency-Key header, or "" if it
	// sent none. Never returned: the client already knows it.
	IdempotencyKey string `json:"-"`

	// Webhook is set on webhook jobs only.
	Webhook *Webhook `json:"webhook,omitempty"`
}

// WithoutBody returns j itself if it carries no webhook body, or else a
// copy without it. Lists and live events use it: a page of 100 jobs with
// 64 KiB bodies would be 6 MiB.
func (j *Job) WithoutBody() *Job {
	if j == nil || j.Webhook == nil || j.Webhook.Body == nil {
		return j
	}
	c := *j
	w := *j.Webhook
	w.Body = nil
	c.Webhook = &w
	return &c
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
		Type:        TypeDemo,
		Status:      StatusPending,
		CreatedAt:   now,
		UpdatedAt:   now,
		MaxAttempts: DefaultMaxAttempts,
		Version:     1,
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

// Attempt is one webhook delivery attempt, as recorded and as shown to the
// job's owner (GET /api/v1/jobs/{id}/attempts).
type Attempt struct {
	Attempt    int       `json:"attempt"`
	StartedAt  time.Time `json:"started_at"`
	DurationMS int       `json:"duration_ms"`
	StatusCode int       `json:"status_code,omitempty"` // 0: no response
	Outcome    string    `json:"outcome"`               // delivered, retry or permanent
	Error      string    `json:"error,omitempty"`
}

// Attempt outcomes.
const (
	OutcomeDelivered = "delivered"
	OutcomeRetry     = "retry"
	OutcomePermanent = "permanent"
)

// MaxAttemptError matches the CHECK on webhook_attempts.error.
const MaxAttemptError = 300
