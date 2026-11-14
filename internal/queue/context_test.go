package queue

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// hangingRedis accepts connections and reads everything sent to it, but
// never answers: a Redis that is up but stuck (Phase 17, FAILURES.md case
// 66). It returns a redis:// URL for it.
func hangingRedis(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var conns []net.Conn
	accepted := make(chan net.Conn)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				close(accepted)
				return
			}
			accepted <- c
			go io.Copy(io.Discard, c)
		}
	}()
	go func() {
		for c := range accepted {
			conns = append(conns, c)
		}
		close(done)
	}()
	t.Cleanup(func() {
		ln.Close()
		<-done
		for _, c := range conns {
			c.Close()
		}
	})
	return "redis://" + ln.Addr().String() + "/0"
}

// A send to Redis must end when its context does, not when go-redis's own
// read timeout (2 s) runs out: the relay gives each send 2 s, and a caller
// with less time left must get it back.
func TestEnqueueStopsAtItsDeadline(t *testing.T) {
	q, err := New(hangingRedis(t))
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = q.Enqueue(ctx, "a")
	took := time.Since(start)
	if err == nil {
		t.Fatal("Enqueue to a Redis that never answers succeeded")
	}
	if took > time.Second {
		t.Errorf("Enqueue with a 500ms deadline returned after %v; want under 1s: the context's deadline doesn't reach the Redis call (only ReadTimeout stops it)", took.Round(10*time.Millisecond))
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v; want context.DeadlineExceeded", err)
	}
}

func TestEnqueueStopsWhenCancelled(t *testing.T) {
	q, err := New(hangingRedis(t))
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	err = q.Enqueue(ctx, "a")
	took := time.Since(start)
	if err == nil {
		t.Fatal("Enqueue to a Redis that never answers succeeded")
	}
	if took > time.Second {
		t.Errorf("Enqueue cancelled after 200ms returned after %v; want under 1s: cancelling doesn't reach the Redis call", took.Round(10*time.Millisecond))
	}
}
