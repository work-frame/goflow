package queue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Publish sends payload to everyone subscribed to channel right now (Redis
// PUBLISH). Pub/Sub keeps nothing: a subscriber that isn't connected at
// this moment never sees the message. Use it only for messages that can be
// lost, never for work.
func (q *Queue) Publish(ctx context.Context, channel string, payload []byte) error {
	if err := q.rdb.Publish(ctx, channel, payload).Err(); err != nil {
		return fmt.Errorf("queue: publish to %s: %w", channel, err)
	}
	return nil
}

// Listen subscribes to channel and calls onMessage with every message until
// ctx ends, then returns nil.
//
// onSubscribed runs each time the subscription is established: once at the
// start, and again after every reconnect (go-redis reconnects on its own
// when Redis goes away and comes back). Messages published while the
// subscription was down are gone, so onSubscribed is the moment to tell
// listeners they may have missed some.
//
// Both callbacks run on Listen's goroutine, one at a time, and must not
// block for long: while they run, incoming messages queue up in a buffer
// of 256 and are dropped by go-redis after a minute.
func (q *Queue) Listen(ctx context.Context, channel string, onSubscribed func(), onMessage func([]byte)) error {
	ps := q.rdb.Subscribe(ctx, channel)
	defer ps.Close()

	msgs := ps.ChannelWithSubscriptions(redis.WithChannelSize(256))
	for {
		select {
		case <-ctx.Done():
			return nil
		case m, ok := <-msgs:
			if !ok {
				return errors.New("queue: subscription closed")
			}
			switch m := m.(type) {
			case *redis.Subscription:
				if m.Kind == "subscribe" {
					onSubscribed()
				}
			case *redis.Message:
				onMessage([]byte(m.Payload))
			}
		}
	}
}

// ErrKeyExists is returned by PutOnce when the key is already taken.
var ErrKeyExists = errors.New("queue: key already exists")

// PutOnce stores value under key for ttl, only if the key doesn't exist
// yet (SET NX EX). Redis deletes it when ttl passes.
func (q *Queue) PutOnce(ctx context.Context, key, value string, ttl time.Duration) error {
	ok, err := q.rdb.SetNX(ctx, key, value, ttl).Result()
	if err != nil {
		return fmt.Errorf("queue: put: %w", err)
	}
	if !ok {
		return ErrKeyExists
	}
	return nil
}

// TakeOnce returns the value stored under key and deletes it in the same
// command (GETDEL), so two callers racing for one key can't both get it.
// found is false if the key doesn't exist or has expired.
func (q *Queue) TakeOnce(ctx context.Context, key string) (value string, found bool, err error) {
	value, err = q.rdb.GetDel(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("queue: take: %w", err)
	}
	return value, true, nil
}
