package queue

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestListenReceivesPublishedMessages(t *testing.T) {
	q := openTestQueue(t)
	channel := "goflow:test:" + t.Name()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	subscribed := make(chan struct{}, 1)
	got := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- q.Listen(ctx, channel,
			func() { subscribed <- struct{}{} },
			func(b []byte) { got <- string(b) })
	}()

	select {
	case <-subscribed:
	case <-time.After(5 * time.Second):
		t.Fatal("onSubscribed not called within 5s")
	}
	if err := q.Publish(ctx, channel, []byte("hello")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case m := <-got:
		if m != "hello" {
			t.Errorf("message = %q, want hello", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("message not received within 5s")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Listen returned %v after ctx ended, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Listen did not return after ctx ended")
	}
}

// Pub/Sub keeps nothing: a message sent before anyone listens is gone.
func TestPublishWithNoListenerIsLost(t *testing.T) {
	q := openTestQueue(t)
	channel := "goflow:test:" + t.Name()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := q.Publish(ctx, channel, []byte("too early")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	subscribed := make(chan struct{}, 1)
	got := make(chan string, 1)
	go q.Listen(ctx, channel, func() { subscribed <- struct{}{} }, func(b []byte) { got <- string(b) })
	<-subscribed
	select {
	case m := <-got:
		t.Fatalf("received %q, published before the subscription", m)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestTakeOnceHasOneWinner(t *testing.T) {
	q := openTestQueue(t)
	ctx := context.Background()
	key := "goflow:test:" + t.Name()
	t.Cleanup(func() { q.rdb.Del(ctx, key) })

	if err := q.PutOnce(ctx, key, "alice", time.Minute); err != nil {
		t.Fatalf("PutOnce: %v", err)
	}
	if err := q.PutOnce(ctx, key, "mallory", time.Minute); !errors.Is(err, ErrKeyExists) {
		t.Fatalf("second PutOnce = %v, want ErrKeyExists", err)
	}

	const n = 10
	var wg sync.WaitGroup
	var mu sync.Mutex
	var winners []string
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, found, err := q.TakeOnce(ctx, key)
			if err != nil {
				t.Errorf("TakeOnce: %v", err)
				return
			}
			if found {
				mu.Lock()
				winners = append(winners, v)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(winners) != 1 || winners[0] != "alice" {
		t.Fatalf("winners = %v, want exactly [alice]", winners)
	}
}

func TestPutOnceExpires(t *testing.T) {
	q := openTestQueue(t)
	ctx := context.Background()
	key := "goflow:test:" + t.Name()
	t.Cleanup(func() { q.rdb.Del(ctx, key) })

	if err := q.PutOnce(ctx, key, "v", 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if _, found, err := q.TakeOnce(ctx, key); err != nil || found {
		t.Fatalf("TakeOnce after expiry = found %v, %v; want not found", found, err)
	}
}
