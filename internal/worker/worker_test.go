package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/work-frame/goflow/internal/database"
	"github.com/work-frame/goflow/internal/queue"
)

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// memQueue behaves like the Redis lists: Take moves an ID from pending to
// processing, Ack removes it, Requeue moves one back to the front. It also
// keeps lease deadlines like the sorted set.
type memQueue struct {
	mu         sync.Mutex
	pending    []string // index 0 is taken first
	processing []string
	delayed    map[string]time.Time
	leases     map[string]time.Time
	calls      *callLog // shared with a memStore to check call order
}

func (q *memQueue) TrackLease(_ context.Context, id string, until time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.leases == nil {
		q.leases = map[string]time.Time{}
	}
	q.leases[id] = until
	return nil
}

func (q *memQueue) DropLease(_ context.Context, id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.leases, id)
	return nil
}

func (q *memQueue) leaseOf(id string) (time.Time, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	t, ok := q.leases[id]
	return t, ok
}

// Schedule records the retry. Tests promote it by hand with promote.
func (q *memQueue) Schedule(_ context.Context, id string, at time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.calls != nil {
		q.calls.add("redis:schedule " + id)
	}
	if q.delayed == nil {
		q.delayed = map[string]time.Time{}
	}
	q.delayed[id] = at
	return nil
}

// promote moves every scheduled ID to pending, like the promoter once due.
func (q *memQueue) promote() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for id := range q.delayed {
		q.pending = append(q.pending, id)
		delete(q.delayed, id)
	}
}

func (q *memQueue) scheduled() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	var ids []string
	for id := range q.delayed {
		ids = append(ids, id)
	}
	return ids
}

func (q *memQueue) Take(ctx context.Context, timeout time.Duration) (string, error) {
	q.mu.Lock()
	if len(q.pending) > 0 {
		id := q.pending[0]
		q.pending = q.pending[1:]
		q.processing = append(q.processing, id)
		q.mu.Unlock()
		return id, nil
	}
	q.mu.Unlock()
	select { // nothing waiting: block briefly, like BLMOVE
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(2 * time.Millisecond):
		return "", queue.ErrEmpty
	}
}

func (q *memQueue) Ack(_ context.Context, id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if i := slices.Index(q.processing, id); i >= 0 {
		q.processing = slices.Delete(q.processing, i, i+1)
	}
	return nil
}

func (q *memQueue) Taken(context.Context) ([]string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.processing), nil
}

func (q *memQueue) Requeue(_ context.Context, id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if i := slices.Index(q.processing, id); i >= 0 {
		q.processing = slices.Delete(q.processing, i, i+1)
		q.pending = append([]string{id}, q.pending...)
	}
	return nil
}

func (q *memQueue) add(ids ...string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending = append(q.pending, ids...)
}

func (q *memQueue) lists() (pending, processing []string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.pending), slices.Clone(q.processing)
}

// memStore tracks statuses and leases like WorkStore: Claim wins only from
// pending, queued or retrying, under a mutex, so exactly one caller wins,
// and gives each claim its own token; every later write needs the job's
// current token. It records the order of Retry calls in calls, shared with
// memQueue's Schedule.
type memStore struct {
	mu       sync.Mutex
	status   map[string]string
	attempts map[string]int
	max      int
	retryErr error // when set, Retry fails (the "commit" fails)
	renewErr error // when set, Renew fails like an unreachable database
	calls    *callLog
	titles   map[string]string
	errors   map[string]string // last error per job, from Retry and Bury
	tokens   map[string]string // the job's current lease token
	claims   int
	renewals int
	refused  []string // writes refused for a stale token
}

func newMemStore(ids ...string) *memStore {
	s := &memStore{status: map[string]string{}, attempts: map[string]int{}, max: 3, calls: &callLog{},
		titles: map[string]string{}, errors: map[string]string{}, tokens: map[string]string{}}
	for _, id := range ids {
		s.status[id] = "queued"
	}
	return s
}

func (s *memStore) Claim(_ context.Context, id string, leaseFor time.Duration) (database.Claimed, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.status[id] {
	case "pending", "queued", "retrying":
		s.status[id] = "processing"
		s.attempts[id]++
		s.claims++
		token := fmt.Sprintf("lease-%d", s.claims)
		s.tokens[id] = token
		return database.Claimed{Attempt: s.attempts[id], MaxAttempts: s.max, Title: s.titles[id],
			LeaseToken: token, LeaseExpires: time.Now().Add(leaseFor)}, true, nil
	}
	return database.Claimed{}, false, nil
}

// owns reports whether token is job id's current lease. s.mu must be held.
func (s *memStore) owns(id, token, op string) bool {
	if s.status[id] == "processing" && s.tokens[id] == token {
		return true
	}
	s.refused = append(s.refused, op+" "+id)
	return false
}

func (s *memStore) Renew(_ context.Context, id, token string, leaseFor time.Duration) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renewals++
	if s.renewErr != nil {
		return time.Time{}, s.renewErr
	}
	if !s.owns(id, token, "renew") {
		return time.Time{}, database.ErrLeaseLost
	}
	return time.Now().Add(leaseFor), nil
}

// takeOver plays another worker taking the job after its lease ran out:
// still processing, under a token the first holder doesn't have.
func (s *memStore) takeOver(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[id] = "another-workers-lease"
}

func (s *memStore) Processing(_ context.Context, ids []string) (map[string]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]bool{}
	for _, id := range ids {
		if s.status[id] == "processing" {
			out[id] = true
		}
	}
	return out, nil
}

func (s *memStore) Complete(_ context.Context, id, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.owns(id, token, "complete") {
		return database.ErrLeaseLost
	}
	s.status[id] = "completed"
	delete(s.tokens, id)
	return nil
}

func (s *memStore) Retry(_ context.Context, id, token string, delay time.Duration, lastError string) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls.add("postgres:retry " + id)
	if s.retryErr != nil {
		return time.Time{}, s.retryErr
	}
	if !s.owns(id, token, "retry") {
		return time.Time{}, database.ErrLeaseLost
	}
	s.errors[id] = lastError
	s.status[id] = "retrying"
	delete(s.tokens, id)
	return time.Now().Add(delay), nil
}

func (s *memStore) Bury(_ context.Context, id, token, lastError string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.owns(id, token, "bury") {
		return database.ErrLeaseLost
	}
	s.errors[id] = lastError
	s.status[id] = "dead"
	delete(s.tokens, id)
	return nil
}

func (s *memStore) tokenOf(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokens[id]
}

func (s *memStore) refusedWrites() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.refused)
}

func (s *memStore) renewCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.renewals
}

func (s *memStore) lastError(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.errors[id]
}

// addJob registers a queued job with a title, for handlers that read it.
func (s *memStore) addJob(id, title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status[id] = "queued"
	s.titles[id] = title
}

func (s *memStore) get(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status[id]
}

func (s *memStore) attemptsOf(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts[id]
}

// callLog records calls across fakes, in order.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(c string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, c)
}

func (l *callLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

// counter counts handler calls per job ID.
type counter struct {
	mu    sync.Mutex
	calls map[string]int
}

func (c *counter) handle(_ context.Context, r Run) error {
	id := r.JobID
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[id]++
	return nil
}

func (c *counter) count(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[id]
}

func newPool(q *memQueue, s *memStore, workers int, h Handler) *Pool {
	return &Pool{Queue: q, Store: s, Handle: h, Logger: discardLogger(), Workers: workers, DrainTimeout: time.Second}
}

// start runs p in the background and returns a function that stops it
// (cancels its context, the SIGTERM equivalent) and waits for Run to return.
func start(t *testing.T, p *Pool) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Run: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("Run did not return within 10s of stopping")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("after 5s: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPoolCompletesAndAcks(t *testing.T) {
	q, s, c := &memQueue{pending: []string{"job1"}}, newMemStore("job1"), &counter{}
	start(t, newPool(q, s, 2, c.handle))

	eventually(t, "job1 completed and acknowledged", func() bool {
		_, processing := q.lists()
		return s.get("job1") == "completed" && len(processing) == 0
	})
	if c.count("job1") != 1 {
		t.Errorf("handler ran %d times, want 1", c.count("job1"))
	}
}

// With 4 workers, 4 jobs run at the same time: each handler waits until all
// four have started. With fewer workers this would never finish.
func TestPoolRunsJobsConcurrently(t *testing.T) {
	ids := []string{"a", "b", "c", "d"}
	q, s := &memQueue{pending: slices.Clone(ids)}, newMemStore(ids...)

	var started sync.WaitGroup
	started.Add(len(ids))
	allStarted := make(chan struct{})
	go func() { started.Wait(); close(allStarted) }()

	start(t, newPool(q, s, len(ids), func(ctx context.Context, _ Run) error {
		started.Done()
		select {
		case <-allStarted:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}))

	eventually(t, "all four jobs completed together", func() bool {
		for _, id := range ids {
			if s.get(id) != "completed" {
				return false
			}
		}
		return true
	})
}

// The same ID twice (at-least-once delivery), taken by different workers:
// the job runs once.
func TestPoolRunsADuplicateIDOnce(t *testing.T) {
	q, s, c := &memQueue{pending: []string{"dup", "dup", "last"}}, newMemStore("dup", "last"), &counter{}
	start(t, newPool(q, s, 3, c.handle))

	eventually(t, "every ID handled", func() bool {
		pending, processing := q.lists()
		return s.get("last") == "completed" && len(pending) == 0 && len(processing) == 0
	})
	if got := c.count("dup"); got != 1 {
		t.Fatalf("duplicate job ran %d times, want 1", got)
	}
}

// A permanent error buries the job at once: no retry is scheduled.
func TestPermanentErrorBuriesTheJob(t *testing.T) {
	q, s := &memQueue{pending: []string{"bad"}}, newMemStore("bad")
	start(t, newPool(q, s, 1, func(context.Context, Run) error {
		return Permanent(errors.New("invalid email address"))
	}))

	eventually(t, "bad marked dead and acknowledged", func() bool {
		_, processing := q.lists()
		return s.get("bad") == "dead" && len(processing) == 0
	})
	if len(q.scheduled()) != 0 {
		t.Errorf("a permanent failure was scheduled for retry: %v", q.scheduled())
	}
}

// A retryable error records the retry, schedules it, and the job runs again
// once due; it succeeds on the second attempt.
func TestRetryableErrorRunsAgain(t *testing.T) {
	q, s := &memQueue{pending: []string{"flaky"}}, newMemStore("flaky")
	var mu sync.Mutex
	runs := 0
	p := newPool(q, s, 1, func(context.Context, Run) error {
		mu.Lock()
		defer mu.Unlock()
		runs++
		if runs == 1 {
			return errors.New("smtp: connection refused")
		}
		return nil
	})
	var gotDelayFor []int
	p.Backoff = func(attempt int) time.Duration {
		mu.Lock()
		defer mu.Unlock()
		gotDelayFor = append(gotDelayFor, attempt)
		return time.Minute
	}
	start(t, p)

	eventually(t, "the retry recorded and scheduled", func() bool {
		return s.get("flaky") == "retrying" && slices.Equal(q.scheduled(), []string{"flaky"})
	})
	q.promote() // the promoter, once the retry is due
	eventually(t, "flaky completed on attempt 2", func() bool { return s.get("flaky") == "completed" })
	if s.attemptsOf("flaky") != 2 {
		t.Errorf("attempts = %d, want 2", s.attemptsOf("flaky"))
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(gotDelayFor, []int{1}) {
		t.Errorf("Backoff called for attempts %v, want [1]", gotDelayFor)
	}
}

// After max attempts the job is dead, not retried again.
func TestOutOfAttemptsBuriesTheJob(t *testing.T) {
	q, s := &memQueue{pending: []string{"doomed"}}, newMemStore("doomed")
	p := newPool(q, s, 1, func(context.Context, Run) error { return errors.New("still down") })
	p.Backoff = func(int) time.Duration { return time.Millisecond }
	start(t, p)

	for range s.max - 1 {
		eventually(t, "a retry scheduled", func() bool { return len(q.scheduled()) == 1 })
		q.promote()
	}
	eventually(t, "doomed dead after 3 attempts", func() bool { return s.get("doomed") == "dead" })
	if s.attemptsOf("doomed") != 3 {
		t.Errorf("attempts = %d, want 3", s.attemptsOf("doomed"))
	}
	if len(q.scheduled()) != 0 {
		t.Errorf("a dead job still has a retry scheduled")
	}
}

// PostgreSQL records the retry before Redis schedules it. If PostgreSQL
// fails, nothing is scheduled and the ID stays in processing: Redis must
// never hold a retry the source of truth doesn't know about.
func TestRetryIsRecordedBeforeItIsScheduled(t *testing.T) {
	q, s := &memQueue{pending: []string{"ok"}}, newMemStore("ok", "commit-fails")
	q.calls = s.calls
	p := newPool(q, s, 1, func(context.Context, Run) error { return errors.New("boom") })
	p.Backoff = func(int) time.Duration { return time.Minute }
	start(t, p)

	eventually(t, "ok retried", func() bool { return s.get("ok") == "retrying" })
	if got, want := s.calls.all(), []string{"postgres:retry ok", "redis:schedule ok"}; !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v: PostgreSQL first, then Redis", got, want)
	}

	s.mu.Lock()
	s.retryErr = errors.New("commit failed")
	s.mu.Unlock()
	q.add("commit-fails")
	eventually(t, "commit-fails attempted", func() bool { return slices.Contains(s.calls.all(), "postgres:retry commit-fails") })
	time.Sleep(20 * time.Millisecond)
	if slices.Contains(s.calls.all(), "redis:schedule commit-fails") {
		t.Fatal("the retry was scheduled in Redis although PostgreSQL didn't record it")
	}
	if _, processing := q.lists(); !slices.Contains(processing, "commit-fails") {
		t.Errorf("processing list = %v, want commit-fails kept for recovery", processing)
	}
}

func TestBackoffGrowsWithJitterAndACap(t *testing.T) {
	for attempt, full := range map[int]time.Duration{1: 2 * time.Second, 2: 4 * time.Second, 5: 32 * time.Second, 9: 5 * time.Minute, 30: 5 * time.Minute} {
		seen := map[time.Duration]bool{}
		for range 200 {
			d := Backoff(attempt)
			if d < full/2 || d > full {
				t.Fatalf("Backoff(%d) = %v, want between %v and %v", attempt, d, full/2, full)
			}
			seen[d] = true
		}
		if len(seen) < 50 {
			t.Errorf("Backoff(%d) gave only %d different values in 200 calls: no jitter", attempt, len(seen))
		}
	}
}

func TestIsPermanentSeesThroughWrapping(t *testing.T) {
	err := fmt.Errorf("send welcome email: %w", Permanent(errors.New("invalid address")))
	if !IsPermanent(err) {
		t.Error("IsPermanent missed a wrapped permanent error")
	}
	if IsPermanent(errors.New("timeout")) || IsPermanent(nil) || Permanent(nil) != nil {
		t.Error("plain errors and nil must not be permanent")
	}
}

// SIGTERM stops new claims but lets an active job finish. The active job's
// context must stay alive, and a job queued after the stop is never taken.
func TestStopLetsTheActiveJobFinish(t *testing.T) {
	q, s := &memQueue{pending: []string{"active"}}, newMemStore("active", "later")

	inJob := make(chan struct{})
	release := make(chan struct{})
	var ctxErrAtEnd error
	p := newPool(q, s, 1, func(ctx context.Context, _ Run) error {
		close(inJob)
		<-release
		ctxErrAtEnd = ctx.Err()
		return nil
	})
	p.DrainTimeout = 5 * time.Second
	stop := start(t, p)

	<-inJob
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	time.Sleep(20 * time.Millisecond) // the stop signal is out; the job is still running
	q.add("later")
	close(release)
	<-stopped

	if ctxErrAtEnd != nil {
		t.Fatalf("the active job's context was cancelled (%v): SIGTERM must not cancel running jobs", ctxErrAtEnd)
	}
	if got := s.get("active"); got != "completed" {
		t.Errorf("active job status = %s, want completed", got)
	}
	if got := s.get("later"); got != "queued" {
		t.Errorf("a job queued after the stop has status %s, want queued: it must not be taken", got)
	}
}

// A job still running when the drain timeout ends is cancelled and records
// nothing. Its lease then runs out and the reaper sends it elsewhere.
func TestDrainTimeoutCancelsAndKeepsTheJob(t *testing.T) {
	q, s := &memQueue{pending: []string{"slow"}}, newMemStore("slow")
	inJob := make(chan struct{})
	p := newPool(q, s, 1, func(ctx context.Context, _ Run) error {
		close(inJob)
		<-ctx.Done() // only returns once cancelled
		return ctx.Err()
	})
	p.DrainTimeout = 50 * time.Millisecond
	stop := start(t, p)

	<-inJob
	began := time.Now()
	stop()
	if took := time.Since(began); took < 50*time.Millisecond {
		t.Errorf("Run returned after %v, before the 50ms drain timeout", took)
	}
	if got := s.get("slow"); got != "processing" {
		t.Errorf("status after the drain timeout = %s, want processing (nothing recorded)", got)
	}
	if _, processing := q.lists(); !slices.Equal(processing, []string{"slow"}) {
		t.Errorf("processing list = %v, want [slow]: the ID must survive for the next start", processing)
	}
}

// Startup cleanup: an ID a stopped worker took but never claimed (its job
// is still queued) goes back to the queue and runs.
func TestStartupRequeuesTakenButUnclaimedIDs(t *testing.T) {
	q := &memQueue{processing: []string{"leftover"}}
	s := newMemStore("leftover")
	c := &counter{}
	start(t, newPool(q, s, 2, c.handle))

	eventually(t, "leftover completed after the cleanup", func() bool { return s.get("leftover") == "completed" })
	if c.count("leftover") != 1 {
		t.Errorf("leftover ran %d times, want 1", c.count("leftover"))
	}
}

// Startup cleanup must never touch a job someone holds a lease on: another
// process may be running it right now. Its stale list entry goes; the job
// stays processing under the same lease, and this pool doesn't run it.
func TestStartupLeavesOtherWorkersJobsAlone(t *testing.T) {
	q := &memQueue{processing: []string{"theirs"}}
	s := newMemStore("theirs")
	if _, _, err := s.Claim(context.Background(), "theirs", time.Minute); err != nil { // another process's claim
		t.Fatal(err)
	}
	theirToken := s.tokenOf("theirs")
	c := &counter{}
	start(t, newPool(q, s, 2, c.handle))

	eventually(t, "the stale list entry removed", func() bool { _, processing := q.lists(); return len(processing) == 0 })
	time.Sleep(20 * time.Millisecond) // give the pool every chance to run it
	if c.count("theirs") != 0 {
		t.Fatal("startup ran a job another worker holds a lease on")
	}
	if s.get("theirs") != "processing" || s.tokenOf("theirs") != theirToken {
		t.Fatalf("startup changed another worker's job: %s under %q", s.get("theirs"), s.tokenOf("theirs"))
	}
	if pending, _ := q.lists(); len(pending) != 0 {
		t.Errorf("pending = %v: another worker's job must not be queued again", pending)
	}
}

// While a job runs, its lease is renewed and its deadline in Redis kept
// up to date; once it finishes, the deadline is dropped.
func TestHeartbeatRenewsTheLease(t *testing.T) {
	q, s := &memQueue{pending: []string{"long"}}, newMemStore("long")
	sawDeadline := make(chan time.Time, 1)
	p := newPool(q, s, 1, func(ctx context.Context, _ Run) error {
		time.Sleep(150 * time.Millisecond) // several heartbeats long, more than one lease
		until, _ := q.leaseOf("long")
		sawDeadline <- until
		return nil
	})
	p.Lease, p.HeartbeatEvery = 60*time.Millisecond, 20*time.Millisecond
	start(t, p)

	eventually(t, "long completed", func() bool { return s.get("long") == "completed" })
	if n := s.renewCount(); n < 4 {
		t.Errorf("%d renewals during a 150ms job with a 20ms heartbeat, want at least 4", n)
	}
	if until := <-sawDeadline; time.Until(until) < -50*time.Millisecond {
		t.Errorf("deadline in Redis was %v ago at the end of the job, want it kept fresh", -time.Until(until))
	}
	if _, ok := q.leaseOf("long"); ok {
		t.Error("the deadline is still tracked after the job finished")
	}
	if refused := s.refusedWrites(); len(refused) != 0 {
		t.Errorf("writes refused: %v", refused)
	}
}

// A worker whose lease is gone (taken back and claimed elsewhere) hears it
// at its next heartbeat: its handler is cancelled, and it records nothing
// at all. The job stays the other worker's.
func TestLostLeaseCancelsTheJobAndRecordsNothing(t *testing.T) {
	q, s := &memQueue{pending: []string{"contested"}}, newMemStore("contested")
	inJob := make(chan struct{})
	cause := make(chan error, 1)
	p := newPool(q, s, 1, func(ctx context.Context, _ Run) error {
		close(inJob)
		<-ctx.Done()
		cause <- context.Cause(ctx)
		return ctx.Err()
	})
	p.Lease, p.HeartbeatEvery = time.Minute, 10*time.Millisecond
	start(t, p)

	<-inJob
	s.takeOver("contested")
	select {
	case got := <-cause:
		if !errors.Is(got, errLeaseLost) {
			t.Errorf("handler cancelled with %v, want errLeaseLost", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler wasn't cancelled within 5s of losing its lease")
	}
	eventually(t, "the ID acknowledged", func() bool { _, processing := q.lists(); return len(processing) == 0 })
	if s.get("contested") != "processing" || s.tokenOf("contested") != "another-workers-lease" {
		t.Errorf("job is %s under %q: the stale worker changed it", s.get("contested"), s.tokenOf("contested"))
	}
	if refused := s.refusedWrites(); !slices.Equal(refused, []string{"renew contested"}) {
		t.Errorf("refused writes %v, want only the heartbeat's renew: it must not try to record anything", refused)
	}
}

// If the database can't be reached for a whole lease length, the worker
// can't know whether its job was taken back, so it assumes so and stops.
func TestUnrenewableLeaseCancelsTheJob(t *testing.T) {
	q, s := &memQueue{pending: []string{"cut-off"}}, newMemStore("cut-off")
	s.renewErr = errors.New("dial tcp: connection refused")
	cancelled := make(chan time.Duration, 1)
	p := newPool(q, s, 1, func(ctx context.Context, _ Run) error {
		began := time.Now()
		<-ctx.Done()
		cancelled <- time.Since(began)
		return ctx.Err()
	})
	p.Lease, p.HeartbeatEvery = 60*time.Millisecond, 10*time.Millisecond
	start(t, p)

	select {
	case after := <-cancelled:
		if after < 50*time.Millisecond {
			t.Errorf("cancelled after %v, before a whole lease (60ms) of failed renewals", after)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the job wasn't cancelled within 5s of its lease running out unrenewed")
	}
	if got := s.get("cut-off"); got != "processing" {
		t.Errorf("status %s, want processing: nothing may be recorded", got)
	}
}

// The race with completion: the handler finishes just as the lease is
// taken over, before any heartbeat notices. The write is refused; the
// result is discarded, not retried, and the job stays the other worker's.
func TestCompletionAfterTheLeaseWasTakenIsDiscarded(t *testing.T) {
	q, s := &memQueue{pending: []string{"late"}}, newMemStore("late")
	var observed []string
	var mu sync.Mutex
	p := newPool(q, s, 1, func(ctx context.Context, _ Run) error {
		s.takeOver("late") // the reaper and another worker moved while we worked
		return nil
	})
	p.Lease, p.HeartbeatEvery = time.Minute, time.Minute // no heartbeat before Complete
	p.Observe = func(outcome string, _ time.Duration) { mu.Lock(); observed = append(observed, outcome); mu.Unlock() }
	start(t, p)

	eventually(t, "the refused completion", func() bool { return slices.Contains(s.refusedWrites(), "complete late") })
	eventually(t, "the ID acknowledged", func() bool { _, processing := q.lists(); return len(processing) == 0 })
	if s.get("late") != "processing" || s.tokenOf("late") != "another-workers-lease" {
		t.Errorf("job is %s under %q, want the other worker's", s.get("late"), s.tokenOf("late"))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(observed) != 0 {
		t.Errorf("observed %v: a discarded result must not count", observed)
	}
}

func TestPoolNeedsAWorker(t *testing.T) {
	err := newPool(&memQueue{}, newMemStore(), 0, (&counter{}).handle).Run(context.Background())
	if err == nil {
		t.Fatal("Run with 0 workers succeeded, want an error")
	}
}

// flakyQueue fails Taken (the startup cleanup's first call) until up is
// set, like a Redis that isn't reachable yet when the process starts.
type flakyQueue struct {
	*memQueue
	mu       sync.Mutex
	up       bool
	attempts int
}

func (q *flakyQueue) Taken(ctx context.Context) ([]string, error) {
	q.mu.Lock()
	q.attempts++
	up := q.up
	q.mu.Unlock()
	if !up {
		return nil, errors.New("dial tcp: lookup red-abc123: no such host")
	}
	return q.memQueue.Taken(ctx)
}

func (q *flakyQueue) state() (attempts int, up bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.attempts, q.up
}

// A pool that starts while Redis is unreachable keeps retrying its startup
// recovery, runs nothing meanwhile, and starts working once Redis is back.
func TestPoolWaitsForRedisAtStartup(t *testing.T) {
	q := &flakyQueue{memQueue: &memQueue{pending: []string{"waiting"}}}
	s, c := newMemStore("waiting"), &counter{}
	p := newPool(nil, s, 2, c.handle)
	p.Queue = q
	p.RetryWait = time.Millisecond
	start(t, p)

	eventually(t, "a few failed recoveries", func() bool { n, _ := q.state(); return n >= 3 })
	if c.count("waiting") != 0 {
		t.Fatal("a job ran before startup recovery succeeded")
	}

	q.mu.Lock()
	q.up = true
	q.mu.Unlock()
	eventually(t, "the waiting job completed once Redis was up", func() bool { return s.get("waiting") == "completed" })
}

// Stopping while still waiting for Redis returns promptly and cleanly.
func TestPoolStopsWhileWaitingForRedis(t *testing.T) {
	q := &flakyQueue{memQueue: &memQueue{}}
	p := newPool(nil, newMemStore(), 1, (&counter{}).handle)
	p.Queue = q
	p.RetryWait = time.Hour
	stop := start(t, p)

	eventually(t, "the first failed recovery", func() bool { n, _ := q.state(); return n >= 1 })
	began := time.Now()
	stop()
	if took := time.Since(began); took > time.Second {
		t.Errorf("stop took %v while waiting for Redis, want well under 1s", took)
	}
}

// A handler that hangs is cancelled at the job timeout, and the attempt
// counts as an ordinary failure: retried, with "timed out" recorded. The
// worker is free for the next job.
func TestJobTimeoutRetriesAHangingJob(t *testing.T) {
	q, s := &memQueue{pending: []string{"stuck", "next"}}, newMemStore("stuck", "next")
	var mu sync.Mutex
	ran := map[string]bool{}
	p := newPool(q, s, 1, func(ctx context.Context, r Run) error {
		mu.Lock()
		ran[r.JobID] = true
		mu.Unlock()
		if r.JobID == "stuck" {
			<-ctx.Done() // hangs until its deadline
			return ctx.Err()
		}
		return nil
	})
	p.JobTimeout = 20 * time.Millisecond
	p.Backoff = func(int) time.Duration { return time.Hour }
	start(t, p)

	eventually(t, "stuck timed out and retrying, next completed", func() bool {
		return s.get("stuck") == "retrying" && s.get("next") == "completed"
	})
	if e := s.lastError("stuck"); !strings.Contains(e, "timed out after 20ms") {
		t.Errorf("last error = %q, want it to say timed out after 20ms", e)
	}
}

// A job that finishes late but successfully counts as done: retrying it
// would run its work twice.
func TestLateSuccessCountsAsDone(t *testing.T) {
	q, s := &memQueue{pending: []string{"slow"}}, newMemStore("slow")
	p := newPool(q, s, 1, func(ctx context.Context, _ Run) error {
		time.Sleep(40 * time.Millisecond) // ignores ctx, then succeeds
		return nil
	})
	p.JobTimeout = 10 * time.Millisecond
	start(t, p)

	eventually(t, "slow completed", func() bool { return s.get("slow") == "completed" })
}

// The demo handler: each title picks a behaviour.
func TestDemoHandler(t *testing.T) {
	h := Demo(time.Millisecond)
	ctx := context.Background()
	tests := []struct {
		title     string
		attempt   int
		wantErr   bool
		permanent bool
	}{
		{"send welcome email", 1, false, false},
		{"sleep:5ms", 1, false, false},
		{"sleep:soon", 1, true, true},
		{"fail", 1, true, false},
		{"fail:permanent", 1, true, true},
		{"flaky:2", 1, true, false},
		{"flaky:2", 2, true, false},
		{"flaky:2", 3, false, false},
		{"flaky:x", 1, true, true},
	}
	for _, tc := range tests {
		err := h(ctx, Run{JobID: "j", Title: tc.title, Attempt: tc.attempt})
		if (err != nil) != tc.wantErr || IsPermanent(err) != tc.permanent {
			t.Errorf("%q attempt %d: err = %v (permanent %v), want error %v, permanent %v",
				tc.title, tc.attempt, err, IsPermanent(err), tc.wantErr, tc.permanent)
		}
	}

	hangCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if err := h(hangCtx, Run{Title: "hang"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("hang: err = %v, want the context's deadline error", err)
	}
}

// A flaky job runs to completion through the pool: fails twice, then succeeds.
func TestPoolRunsAFlakyJobToCompletion(t *testing.T) {
	q, s := &memQueue{}, newMemStore()
	s.addJob("f", "flaky:2")
	q.add("f")
	p := newPool(q, s, 1, Demo(time.Millisecond))
	p.Backoff = func(int) time.Duration { return time.Millisecond }
	start(t, p)

	for range 2 {
		eventually(t, "a retry scheduled", func() bool { return len(q.scheduled()) == 1 })
		q.promote()
	}
	eventually(t, "f completed on attempt 3", func() bool { return s.get("f") == "completed" })
	if s.attemptsOf("f") != 3 {
		t.Errorf("attempts = %d, want 3", s.attemptsOf("f"))
	}
}

// Observe hears every attempt that ended, with its outcome, after the
// outcome is saved.
func TestObserveHearsEachOutcome(t *testing.T) {
	q, s := &memQueue{pending: []string{"ok", "flaky", "bad"}}, newMemStore("ok", "flaky", "bad")
	var mu sync.Mutex
	heard := map[string]string{}
	p := newPool(q, s, 1, func(_ context.Context, r Run) error {
		switch r.JobID {
		case "flaky":
			return errors.New("try again")
		case "bad":
			return Permanent(errors.New("never"))
		}
		return nil
	})
	p.Backoff = func(int) time.Duration { return time.Hour } // flaky stays retrying
	p.Observe = func(outcome string, took time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		heard[outcome] += "x" // one x per observation
		if took < 0 {
			t.Errorf("%s: negative duration %v", outcome, took)
		}
	}
	start(t, p)

	eventually(t, "three outcomes observed", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return heard["completed"] == "x" && heard["retry"] == "x" && heard["dead"] == "x"
	})
}
