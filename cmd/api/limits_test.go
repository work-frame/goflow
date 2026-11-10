package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Login limits on real Redis: the eleventh attempt for one email is
// refused with Retry-After, and Redis holds a counter for it that expires
// and doesn't contain the email.
func TestLoginIsLimitedEndToEnd(t *testing.T) {
	parallelTest(t)
	dbURL := testDatabaseURL(t)
	redisURL := testRedisURL(t)
	cfg := testConfig("127.0.0.1:0", dbURL)
	cfg.RedisURL = redisURL
	base, _ := startServerWith(t, cfg)
	client := testClient(t, 5*time.Second)

	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opts)
	ctx := context.Background()
	t.Cleanup(func() { rdb.Close() })
	t.Cleanup(func() {
		keys, _ := rdb.Keys(ctx, cfg.RateLimitPrefix+"*").Result()
		if len(keys) > 0 {
			rdb.Del(ctx, keys...)
		}
	})

	creds := `{"email":"nobody-e2e@example.com","password":"wrong password!"}`
	for i := 1; i <= 10; i++ {
		if status, _, body := request(t, client, http.MethodPost, base+"/api/v1/auth/login", creds); status != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d, body %s; want 401", i, status, body)
		}
	}
	status, header, body := request(t, client, http.MethodPost, base+"/api/v1/auth/login", creds)
	if status != http.StatusTooManyRequests || header.Get("Retry-After") == "" {
		t.Fatalf("11th attempt: status %d, Retry-After %q, body %s; want 429 with Retry-After", status, header.Get("Retry-After"), body)
	}

	keys, err := rdb.Keys(ctx, cfg.RateLimitPrefix+"*").Result()
	if err != nil {
		t.Fatal(err)
	}
	var emailKey string
	for _, k := range keys {
		if strings.Contains(k, "nobody") || strings.Contains(k, "127.0.0.1") {
			t.Errorf("Redis key %q contains the email or the address", k)
		}
		if strings.Contains(k, ":login_email:") {
			emailKey = k
		}
	}
	if emailKey == "" {
		t.Fatalf("no login_email counter in Redis among %v: is the limiter counting in memory?", keys)
	}
	if ttl := rdb.PTTL(ctx, emailKey).Val(); ttl <= 0 || ttl > 15*time.Minute {
		t.Errorf("counter expires in %v, want within 15 minutes", ttl)
	}
	if n, _ := rdb.Get(ctx, emailKey).Int(); n != 11 {
		t.Errorf("counter = %d, want 11", n)
	}
}
