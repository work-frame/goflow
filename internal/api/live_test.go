package api

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/work-frame/goflow/internal/events"
)

// fakeLive stands in for tickets and the hub. Tickets are plain map
// entries; every stream gets the channel the test pushes to.
type fakeLive struct {
	mu           sync.Mutex
	tickets      map[string]string // ticket → user ID
	issuedFor    []string
	redeemErr    error
	msgs         chan []byte
	subscribedAs string
	unsubscribed chan struct{}
	closed       bool
	subscribed   chan struct{}
	paused       bool
}

// newFakeLive returns a fakeLive whose Redis subscription is already live.
func newFakeLive() *fakeLive {
	f := &fakeLive{tickets: map[string]string{}, msgs: make(chan []byte, 8), unsubscribed: make(chan struct{}), subscribed: make(chan struct{})}
	close(f.subscribed)
	return f
}

func (f *fakeLive) Subscribed() <-chan struct{} { return f.subscribed }

func (f *fakeLive) Paused() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paused
}

func (f *fakeLive) IssueTicket(_ context.Context, userID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issuedFor = append(f.issuedFor, userID)
	t := "gft_test_" + userID
	f.tickets[t] = userID
	return t, nil
}

func (f *fakeLive) RedeemTicket(_ context.Context, ticket string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.redeemErr != nil {
		return "", f.redeemErr
	}
	u, ok := f.tickets[ticket]
	if !ok {
		return "", events.ErrBadTicket
	}
	delete(f.tickets, ticket)
	return u, nil
}

func (f *fakeLive) Subscribe(userID string) (<-chan []byte, func(), bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, func() {}, false
	}
	f.subscribedAs = userID
	var once sync.Once
	return f.msgs, func() { once.Do(func() { close(f.unsubscribed) }) }, true
}

func (f *fakeLive) addTicket(ticket, userID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tickets[ticket] = userID
}

func TestTicketNeedsASession(t *testing.T) {
	router, _, _ := twoUsers(t)
	if rec := do(t, router, http.MethodPost, "/api/v1/events/ticket", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
}

func TestTicketIsIssuedToTheCaller(t *testing.T) {
	live := newFakeLive()
	router := signedIn(Deps{Jobs: newTestStore(), Live: live, DB: okPinger, Logger: discardLogger()})

	rec := do(t, router, http.MethodPost, "/api/v1/events/ticket", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, body %s; want 201", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store: a ticket is a credential", cc)
	}
	got := decode[ticketResponse](t, rec)
	if got.Ticket == "" || got.ExpiresIn != 30 {
		t.Errorf("response %+v, want a ticket and expires_in 30", got)
	}
	if len(live.issuedFor) != 1 || live.issuedFor[0] == "" {
		t.Errorf("ticket issued for %v, want the signed-in user", live.issuedFor)
	}
}

func TestLiveEndpointsWithoutRedisAre503(t *testing.T) {
	router := newRouter() // no Live
	if rec := do(t, router, http.MethodPost, "/api/v1/events/ticket", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("ticket: status %d, want 503", rec.Code)
	}
	if rec := do(t, router, http.MethodGet, "/api/v1/events?ticket=x", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("stream: status %d, want 503", rec.Code)
	}
}

func TestStreamRefusesMissingAndBadTickets(t *testing.T) {
	live := newFakeLive()
	router := NewRouter(Deps{Jobs: newTestStore(), Live: live, DB: okPinger, Logger: discardLogger()})

	for _, path := range []string{"/api/v1/events", "/api/v1/events?ticket=gft_unknown"} {
		if rec := do(t, router, http.MethodGet, path, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s: status %d, want 401", path, rec.Code)
		}
	}

	live.addTicket("gft_once", "alice")
	ok := httptest.NewServer(router)
	t.Cleanup(ok.Close)
	first := openStream(t, ok.URL, "gft_once")
	first.close()
	if code := streamStatus(t, ok.URL, "gft_once"); code != http.StatusUnauthorized {
		t.Errorf("second use of a ticket: status %d, want 401", code)
	}

	live.redeemErr = errors.New("redis down")
	if rec := do(t, router, http.MethodGet, "/api/v1/events?ticket=gft_x", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("Redis down: status %d, want 503", rec.Code)
	}
}

// liveStream reads an SSE response one event at a time.
type liveStream struct {
	resp   *http.Response
	lines  *bufio.Scanner
	cancel context.CancelFunc
}

func openStream(t *testing.T, base, ticket string) *liveStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/events?ticket="+ticket, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("open stream: status %d", resp.StatusCode)
	}
	s := &liveStream{resp: resp, lines: bufio.NewScanner(resp.Body), cancel: cancel}
	t.Cleanup(s.close)
	return s
}

func streamStatus(t *testing.T, base, ticket string) int {
	t.Helper()
	resp, err := http.Get(base + "/api/v1/events?ticket=" + ticket)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (s *liveStream) close() {
	s.cancel()
	s.resp.Body.Close()
}

// next returns the next "data:" payload, or "ping" for a comment line, or
// "EOF" when the server ended the stream.
func (s *liveStream) next(t *testing.T) string {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		for s.lines.Scan() {
			line := s.lines.Text()
			switch {
			case strings.HasPrefix(line, "data: "):
				got <- strings.TrimPrefix(line, "data: ")
				return
			case strings.HasPrefix(line, ":"):
				got <- "ping"
				return
			}
		}
		got <- "EOF"
	}()
	select {
	case line := <-got:
		return line
	case <-time.After(3 * time.Second):
		t.Fatal("no stream line within 3s")
		return ""
	}
}

func TestStreamSendsReadyThenTheOwnersEvents(t *testing.T) {
	live := newFakeLive()
	live.addTicket("gft_alice", "alice")
	srv := httptest.NewServer(NewRouter(Deps{Jobs: newTestStore(), Live: live, DB: okPinger, Logger: discardLogger()}))
	t.Cleanup(srv.Close) // after the streams' own cleanups: Close waits for open handlers

	s := openStream(t, srv.URL, "gft_alice")
	if ct := s.resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	if got := s.next(t); got != `{"type":"ready"}` {
		t.Fatalf("first message %s, want ready", got)
	}
	if live.subscribedAs != "alice" {
		t.Errorf("subscribed as %q, want the ticket's owner alice", live.subscribedAs)
	}
	live.msgs <- []byte(`{"type":"deleted","id":"j1"}`)
	if got := s.next(t); got != `{"type":"deleted","id":"j1"}` {
		t.Errorf("got %s", got)
	}

	close(live.msgs) // the hub dropped the stream
	if got := s.next(t); got != "EOF" {
		t.Errorf("after the hub closed the stream: %s, want EOF", got)
	}
}

func TestStreamEndsWhenTheClientLeaves(t *testing.T) {
	live := newFakeLive()
	live.addTicket("gft_alice", "alice")
	srv := httptest.NewServer(NewRouter(Deps{Jobs: newTestStore(), Live: live, DB: okPinger, Logger: discardLogger()}))
	t.Cleanup(srv.Close) // after the streams' own cleanups: Close waits for open handlers

	s := openStream(t, srv.URL, "gft_alice")
	s.next(t)
	s.close()
	select {
	case <-live.unsubscribed:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not unsubscribe within 3s of the client leaving")
	}
}

// The server's 10-second read and write timeouts would cut every stream
// off. Here they are 100 ms, and the stream must outlive them.
func TestStreamOutlivesServerTimeouts(t *testing.T) {
	live := newFakeLive()
	live.addTicket("gft_alice", "alice")
	srv := httptest.NewUnstartedServer(NewRouter(Deps{
		Jobs: newTestStore(), Live: live, DB: okPinger, Logger: discardLogger(), StreamHeartbeat: 50 * time.Millisecond,
	}))
	srv.Config.ReadTimeout = 100 * time.Millisecond
	srv.Config.WriteTimeout = 100 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close) // after the streams' own cleanups: Close waits for open handlers

	s := openStream(t, srv.URL, "gft_alice")
	s.next(t) // ready
	time.Sleep(400 * time.Millisecond)
	live.msgs <- []byte(`{"type":"resync"}`)
	for {
		got := s.next(t)
		if got == "ping" {
			continue // heartbeats sent while we slept
		}
		if got != `{"type":"resync"}` {
			t.Fatalf("after 400 ms: %s, want the message", got)
		}
		return
	}
}

func TestStreamRefusedWhileShuttingDown(t *testing.T) {
	live := newFakeLive()
	live.closed = true
	live.addTicket("gft_alice", "alice")
	router := NewRouter(Deps{Jobs: newTestStore(), Live: live, DB: okPinger, Logger: discardLogger()})
	if rec := do(t, router, http.MethodGet, "/api/v1/events?ticket=gft_alice", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
}

// "ready" means events can reach the client. Right after the API starts,
// its Redis subscription may not be live yet, and the stream waits.
func TestReadyWaitsForTheRedisSubscription(t *testing.T) {
	live := newFakeLive()
	live.subscribed = make(chan struct{}) // not live yet
	live.addTicket("gft_alice", "alice")
	srv := httptest.NewServer(NewRouter(Deps{
		Jobs: newTestStore(), Live: live, DB: okPinger, Logger: discardLogger(), StreamHeartbeat: 50 * time.Millisecond,
	}))
	t.Cleanup(srv.Close)

	s := openStream(t, srv.URL, "gft_alice") // 200 arrives at once
	live.msgs <- []byte(`{"type":"resync"}`) // dropped: the client hasn't loaded anything yet
	for range 3 {
		if got := s.next(t); got != "ping" {
			t.Fatalf("before the subscription is live: %s, want only pings", got)
		}
	}
	close(live.subscribed)
	for {
		got := s.next(t)
		if got == "ping" {
			continue
		}
		if got != `{"type":"ready"}` {
			t.Fatalf("after the subscription: %s, want ready", got)
		}
		return
	}
}

// A stream that opens while events aren't reaching the API must not look
// healthy: "paused" comes right after "ready", and the stream stays open
// for the "resumed" that ends it.
func TestStreamOpenedDuringAPauseSaysSo(t *testing.T) {
	live := newFakeLive()
	live.paused = true
	live.addTicket("gft_alice", "alice")
	srv := httptest.NewServer(NewRouter(Deps{Jobs: newTestStore(), Live: live, DB: okPinger, Logger: discardLogger()}))
	t.Cleanup(srv.Close) // after the streams' own cleanups: Close waits for open handlers

	s := openStream(t, srv.URL, "gft_alice")
	for _, want := range []string{`{"type":"ready"}`, `{"type":"paused"}`} {
		if got := s.next(t); got != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	}
	live.msgs <- []byte(`{"type":"resumed"}`)
	if got := s.next(t); got != `{"type":"resumed"}` {
		t.Errorf("got %s, want resumed on the same stream", got)
	}
}

// When delivery works, "ready" is all a new stream hears.
func TestStreamOpenedWhileHealthyIsNotPaused(t *testing.T) {
	live := newFakeLive()
	live.addTicket("gft_alice", "alice")
	srv := httptest.NewServer(NewRouter(Deps{Jobs: newTestStore(), Live: live, DB: okPinger, Logger: discardLogger()}))
	t.Cleanup(srv.Close)

	s := openStream(t, srv.URL, "gft_alice")
	s.next(t) // ready
	live.msgs <- []byte(`{"type":"deleted","id":"j1"}`)
	if got := s.next(t); got != `{"type":"deleted","id":"j1"}` {
		t.Errorf("second message %s, want the event (no paused)", got)
	}
}
