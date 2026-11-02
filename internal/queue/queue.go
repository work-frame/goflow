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
	"time"

	"github.com/redis/go-redis/v9"
)

// PendingKey is the Redis list of job IDs waiting for a worker.
// The API pushes on the left (LPUSH); workers (Phase 6) will pop on the
// right, so the oldest ID comes out first.
const PendingKey = "goflow:jobs:pending"

// Queue is a connection pool to Redis plus the list it uses.
// It is safe for concurrent use: go-redis manages its own pool.
type Queue struct {
	rdb *redis.Client
	key string
}

// Open connects to the Redis server at rawURL and checks it with a PING.
//
// The URL can hold a password (redis://:password@host:6379/0), so neither
// the URL nor go-redis's parse error, which may quote parts of it, ever
// goes into the returned error.
func Open(ctx context.Context, rawURL string) (*Queue, error) {
	opts, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, errors.New("queue: GOFLOW_REDIS_URL is not a valid redis:// or rediss:// URL")
	}
	// Fail fast instead of go-redis's longer defaults: a request that waits
	// on a dead Redis should give up well inside its 3-second budget.
	opts.DialTimeout = 2 * time.Second
	opts.ReadTimeout = 2 * time.Second
	opts.WriteTimeout = 2 * time.Second

	q := &Queue{rdb: redis.NewClient(opts), key: PendingKey}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := q.PingContext(pingCtx); err != nil {
		q.Close()
		return nil, fmt.Errorf("queue: ping: %w", err)
	}
	return q, nil
}

// Enqueue adds a job ID to the pending list.
func (q *Queue) Enqueue(ctx context.Context, jobID string) error {
	if err := q.rdb.LPush(ctx, q.key, jobID).Err(); err != nil {
		return fmt.Errorf("queue: enqueue %s: %w", jobID, err)
	}
	return nil
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
