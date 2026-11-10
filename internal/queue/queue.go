// Package queue hands job IDs to workers through a Redis list.
//
// Only IDs travel through Redis. The job itself (title, owner, status) stays
// in PostgreSQL, which remains the source of truth: a worker reads the job
// from the database by its ID. If Redis loses its data, no job is lost,
// only the "please run this" message.
package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// The Redis keys GoFlow's queue uses.
const (
	// PendingKey holds job IDs waiting for a worker. The relay pushes on the
	// left (LPUSH); workers take from the right, so the oldest comes first.
	PendingKey = "goflow:jobs:pending"
	// ProcessingKey holds the IDs a worker has taken but not yet finished.
	// An ID stays here until Ack, so a worker that dies between taking an ID
	// and claiming its job leaves it behind instead of losing it.
	ProcessingKey = "goflow:jobs:processing"
	// DelayedKey is a sorted set of job IDs waiting for a retry, scored by
	// when they are due (Unix milliseconds). It only schedules delivery:
	// PostgreSQL records that a job is retrying and when.
	DelayedKey = "goflow:jobs:delayed"
	// LeasesKey is a sorted set of job IDs scored by when their lease runs
	// out (Unix milliseconds, as PostgreSQL computed it). It only tells the
	// lease reaper when to ask PostgreSQL, which alone decides whether a
	// lease has expired. Losing it costs nothing but time: the reaper's
	// hourly sweep reads PostgreSQL directly.
	LeasesKey = "goflow:jobs:leases"
)

// DefaultPrefix starts the names of the queue's keys: PendingKey and the
// others are DefaultPrefix + "jobs:...". Keys(DefaultPrefix) gives them.
const DefaultPrefix = "goflow:"

// Keys returns the queue's four key names under prefix. Production uses
// DefaultPrefix; tests give every test its own, so two test servers never
// take each other's jobs (Task 54).
func Keys(prefix string) (pending, processing, delayed, leases string) {
	return prefix + "jobs:pending", prefix + "jobs:processing", prefix + "jobs:delayed", prefix + "jobs:leases"
}

// ErrEmpty means Take waited and no job ID arrived.
var ErrEmpty = errors.New("queue: no job waiting")

// Queue is a connection pool to Redis plus the lists it uses.
// It is safe for concurrent use: go-redis manages its own pool.
type Queue struct {
	rdb        *redis.Client
	key        string // pending list
	processing string
	delayed    string
	leases     string
}

// Open connects to the Redis server at rawURL and checks it with a PING.
//
// The URL can hold a password (redis://:password@host:6379/0), so neither
// the URL nor go-redis's parse error, which may quote parts of it, ever
// goes into the returned error.
func Open(ctx context.Context, rawURL string) (*Queue, error) {
	q, err := New(rawURL)
	if err != nil {
		return nil, err
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := q.PingContext(pingCtx); err != nil {
		q.Close()
		return nil, fmt.Errorf("queue: ping: %w", err)
	}
	return q, nil
}

// New returns a Queue for the Redis server at rawURL without connecting:
// go-redis dials on the first command. Use it when Redis may not be up yet
// and the caller retries on its own (the API's relay and workers do). Like
// Open, it never puts the URL or go-redis's parse error into its error.
func New(rawURL string) (*Queue, error) {
	opts, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, errors.New("queue: GOFLOW_REDIS_URL is not a valid redis:// or rediss:// URL")
	}
	// Fail fast instead of go-redis's longer defaults: a request that waits
	// on a dead Redis should give up well inside its 3-second budget.
	opts.DialTimeout = 2 * time.Second
	opts.ReadTimeout = 2 * time.Second
	opts.WriteTimeout = 2 * time.Second
	return &Queue{rdb: redis.NewClient(opts), key: PendingKey, processing: ProcessingKey, delayed: DelayedKey, leases: LeasesKey}, nil
}

// Enqueue adds job IDs to the pending list in one command, keeping their
// order: the first ID given is the first a worker will pop.
func (q *Queue) Enqueue(ctx context.Context, jobIDs ...string) error {
	if len(jobIDs) == 0 {
		return nil
	}
	// LPUSH takes ...any, so the strings are copied into a []any first.
	args := make([]any, len(jobIDs))
	for i, id := range jobIDs {
		args[i] = id
	}
	if err := q.rdb.LPush(ctx, q.key, args...).Err(); err != nil {
		return fmt.Errorf("queue: enqueue %d job(s): %w", len(jobIDs), err)
	}
	return nil
}

// Take waits up to timeout for a job ID and moves it from the pending list
// to the processing list in one atomic step (BLMOVE). It returns ErrEmpty if
// nothing arrived in time.
//
// Moving instead of popping is what makes the queue reliable: at every
// moment the ID is in exactly one of the two lists, never only in a
// worker's memory.
func (q *Queue) Take(ctx context.Context, timeout time.Duration) (string, error) {
	id, err := q.rdb.BLMove(ctx, q.key, q.processing, "RIGHT", "LEFT", timeout).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrEmpty
	}
	if err != nil {
		return "", fmt.Errorf("queue: take: %w", err)
	}
	return id, nil
}

// Ack removes a finished job ID from the processing list. Call it only
// after the job's result is saved: an ID acknowledged too early is lost
// if the worker dies before saving.
func (q *Queue) Ack(ctx context.Context, jobID string) error {
	if err := q.rdb.LRem(ctx, q.processing, 1, jobID).Err(); err != nil {
		return fmt.Errorf("queue: ack %s: %w", jobID, err)
	}
	return nil
}

// Taken returns every ID in the processing list: taken by some worker, in
// this process or another, and not acknowledged yet.
func (q *Queue) Taken(ctx context.Context) ([]string, error) {
	ids, err := q.rdb.LRange(ctx, q.processing, 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("queue: taken: %w", err)
	}
	return ids, nil
}

// requeueScript moves one copy of ARGV[1] from the processing list
// (KEYS[1]) to the end of the pending list (KEYS[2]) that Take reads first.
// Only if it was there: two processes cleaning up at once can't both push
// the same entry back.
var requeueScript = redis.NewScript(`
local n = redis.call('LREM', KEYS[1], 1, ARGV[1])
if n > 0 then
	redis.call('RPUSH', KEYS[2], ARGV[1])
end
return n
`)

// Requeue moves one copy of jobID from the processing list back to the
// front of the queue, so it's taken next. It does nothing if jobID isn't in
// the processing list.
func (q *Queue) Requeue(ctx context.Context, jobID string) error {
	if err := requeueScript.Run(ctx, q.rdb, []string{q.processing, q.key}, jobID).Err(); err != nil {
		return fmt.Errorf("queue: requeue %s: %w", jobID, err)
	}
	return nil
}

// TrackLease records that jobID's lease runs out at until, as PostgreSQL
// computed it. Tracking an ID again moves its deadline.
func (q *Queue) TrackLease(ctx context.Context, jobID string, until time.Time) error {
	if err := q.rdb.ZAdd(ctx, q.leases, redis.Z{Score: float64(until.UnixMilli()), Member: jobID}).Err(); err != nil {
		return fmt.Errorf("queue: track lease of %s: %w", jobID, err)
	}
	return nil
}

// DropLease forgets jobID's lease deadline.
func (q *Queue) DropLease(ctx context.Context, jobID string) error {
	if err := q.rdb.ZRem(ctx, q.leases, jobID).Err(); err != nil {
		return fmt.Errorf("queue: drop lease of %s: %w", jobID, err)
	}
	return nil
}

// ExpiredLeases returns up to limit job IDs whose tracked deadline is at or
// before now, earliest first. now is the caller's clock, so this only says
// which leases are worth asking PostgreSQL about.
func (q *Queue) ExpiredLeases(ctx context.Context, now time.Time, limit int) ([]string, error) {
	ids, err := q.rdb.ZRangeByScore(ctx, q.leases, &redis.ZRangeBy{
		Min: "-inf", Max: strconv.FormatInt(now.UnixMilli(), 10), Count: int64(limit),
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("queue: expired leases: %w", err)
	}
	return ids, nil
}

// WithKeys returns a Queue that shares q's connections but uses other
// keys. Tests use it so they never touch the real ones.
func (q *Queue) WithKeys(pending, processing, delayed, leases string) *Queue {
	return &Queue{rdb: q.rdb, key: pending, processing: processing, delayed: delayed, leases: leases}
}

// WithPrefix returns a Queue on the same connection whose keys start with
// prefix instead of DefaultPrefix (see Keys).
func (q *Queue) WithPrefix(prefix string) *Queue {
	return q.WithKeys(Keys(prefix))
}

// Schedule puts a job ID in the delayed set, due at `at`. Scheduling an ID
// that is already there just moves its due time.
func (q *Queue) Schedule(ctx context.Context, jobID string, at time.Time) error {
	if err := q.rdb.ZAdd(ctx, q.delayed, redis.Z{Score: float64(at.UnixMilli()), Member: jobID}).Err(); err != nil {
		return fmt.Errorf("queue: schedule %s: %w", jobID, err)
	}
	return nil
}

// promoteScript moves up to ARGV[2] IDs due by ARGV[1] (Unix ms) from the
// delayed set (KEYS[1]) to the pending list (KEYS[2]), earliest first.
// Redis runs a script as one step: no other command runs in the middle,
// so two promoters can't move the same ID twice.
var promoteScript = redis.NewScript(`
local ids = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1], 'LIMIT', 0, ARGV[2])
for _, id in ipairs(ids) do
	redis.call('ZREM', KEYS[1], id)
	redis.call('LPUSH', KEYS[2], id)
end
return #ids
`)

// PromoteDue moves up to limit IDs that are due by now from the delayed set
// to the pending list, earliest first, and returns how many it moved.
func (q *Queue) PromoteDue(ctx context.Context, now time.Time, limit int) (int, error) {
	n, err := promoteScript.Run(ctx, q.rdb, []string{q.delayed, q.key}, now.UnixMilli(), limit).Int()
	if err != nil {
		return 0, fmt.Errorf("queue: promote due retries: %w", err)
	}
	return n, nil
}

// Sizes reports how many job IDs wait in the pending list, sit in the
// processing list, and wait in the delayed set, in one round trip.
func (q *Queue) Sizes(ctx context.Context) (pending, processing, delayed int64, err error) {
	pipe := q.rdb.Pipeline()
	p := pipe.LLen(ctx, q.key)
	pr := pipe.LLen(ctx, q.processing)
	d := pipe.ZCard(ctx, q.delayed)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, 0, 0, fmt.Errorf("queue: sizes: %w", err)
	}
	return p.Val(), pr.Val(), d.Val(), nil
}

// Len reports how many job IDs are waiting.
func (q *Queue) Len(ctx context.Context) (int64, error) {
	n, err := q.rdb.LLen(ctx, q.key).Result()
	if err != nil {
		return 0, fmt.Errorf("queue: length: %w", err)
	}
	return n, nil
}

// PingContext checks that Redis answers. The name matches *sql.DB's method,
// so the API's readiness check can treat both the same way.
func (q *Queue) PingContext(ctx context.Context) error {
	return q.rdb.Ping(ctx).Err()
}

// Close closes every connection in the pool.
func (q *Queue) Close() error {
	return q.rdb.Close()
}

// UseLogger sends go-redis's own log lines (connection retries, pool
// errors) through logger instead of go-redis's default, which prints plain
// text to stderr outside GoFlow's structured logs.
//
// go-redis has one logger for the whole process, so this changes it for
// every client. Call it once, at startup.
func UseLogger(logger *slog.Logger) {
	redis.SetLogger(slogAdapter{logger})
}

// slogAdapter satisfies go-redis's logging interface with a *slog.Logger.
type slogAdapter struct{ l *slog.Logger }

func (a slogAdapter) Printf(ctx context.Context, format string, v ...any) {
	a.l.WarnContext(ctx, "redis", "detail", strings.TrimSpace(fmt.Sprintf(format, v...)))
}
