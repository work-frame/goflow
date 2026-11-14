// Package loadgen measures how fast a GoFlow API on this machine gets jobs
// done: it creates jobs through the real API, follows them until each one
// is completed, dead or out of time, and reports jobs completed per second
// and how long each took from create to completion (Task 56).
//
// It only ever talks to an API on this machine (CheckLocal): a load test
// against the hosted API would burn the free database's compute hours and
// get in its users' way.
package loadgen

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/work-frame/goflow/internal/client"
	"github.com/work-frame/goflow/internal/job"
)

// Config is one load run.
type Config struct {
	// API is the API's base URL. It must be on this machine (CheckLocal).
	API string
	// Token is a session token for the account the jobs belong to.
	Token string
	// Jobs is how many jobs to create; Concurrency how many create
	// requests are in flight at once.
	Jobs, Concurrency int
	// Title names job i. Nil means "sleep:0s": the demo handler returns at
	// once, so the run measures GoFlow, not the work.
	Title func(i int) string
	// Wait is how long, after the last create, jobs have to finish. A job
	// still unfinished then is reported as unfinished.
	Wait time.Duration
	// PollEvery is how often the API itself is asked about jobs no event
	// has finished yet (default 1 s); PollBatch caps how many per round
	// (default 50), so the checking doesn't become the load.
	PollEvery time.Duration
	PollBatch int
}

// Report is what a run measured.
type Report struct {
	Requested, Created int
	JobIDs             []string // every job created, for cleaning up afterwards
	CreateErrors       []string // one line per failed create
	Completed          int
	Dead               []string // job IDs that ended dead
	Unfinished         []string // job IDs neither completed nor dead in time
	// CreatesTook runs from the first create request to the last create
	// response; CreateRate is Created / CreatesTook. If creating is slow,
	// it caps PerSecond, whatever the workers do (Task 57).
	CreatesTook time.Duration
	CreateRate  float64
	// Elapsed runs from the first create request to the last completion.
	Elapsed time.Duration
	// PerSecond is Completed / Elapsed. Unfinished and dead jobs aren't in
	// it, and they are listed above, so they can't vanish from the result.
	PerSecond float64
	// P50, P95 and P99 are times from a job's create request to the moment
	// this run saw it completed, over completed jobs only.
	P50, P95, P99 time.Duration
	// SeenByEvent and SeenByPoll say how each finished job was noticed.
	// Events are fast but not guaranteed (Phase 8); polls are slower and
	// authoritative. A job noticed by a poll has its time rounded up to
	// that poll.
	SeenByEvent, SeenByPoll int
}

// CheckLocal refuses any API that isn't on this machine: the host must be
// "localhost" or a loopback address (127.0.0.0/8, ::1). Nothing is sent
// anywhere before this passes.
func CheckLocal(api string) error {
	u, err := url.Parse(api)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("loadgen: %q is not an http:// or https:// URL", api)
	}
	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("loadgen: refusing %s: load tests run only against an API on this machine (localhost, 127.0.0.1 or ::1)", host)
}

// tracked is one created job.
type tracked struct {
	sent time.Time // when its create request went out
	done bool
}

// run holds a run's shared state.
type run struct {
	mu      sync.Mutex
	jobs    map[string]*tracked
	early   map[string]seenEarly // finished before their create response came back
	report  Report
	last    time.Time // the latest completion seen
	latency []time.Duration
	allDone chan struct{} // closed when every created job has finished
	// resync gets a value whenever the events stream was lost or told us
	// it missed messages: then every open job is checked at once instead
	// of waiting for the next poll round.
	resync  chan struct{}
	closed  bool
	created bool // every create request has returned
}

// seenEarly is a finish that arrived before the job's create response.
type seenEarly struct {
	status  job.Status
	seen    time.Time
	byEvent bool
}

// finish records that id ended in status, noticed at seen.
func (r *run) finish(id string, status job.Status, seen time.Time, byEvent bool) {
	if status != job.StatusCompleted && status != job.StatusDead && status != job.StatusFailed {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finishLocked(id, status, seen, byEvent)
}

func (r *run) finishLocked(id string, status job.Status, seen time.Time, byEvent bool) {
	t, ok := r.jobs[id]
	if !ok {
		// Maybe ours, its create response not back yet: kept until it is.
		// (Other jobs of the account end up here too, and stay unused.)
		if !r.created {
			if _, dup := r.early[id]; !dup {
				r.early[id] = seenEarly{status, seen, byEvent}
			}
		}
		return
	}
	if t.done {
		return // already counted
	}
	t.done = true
	if byEvent {
		r.report.SeenByEvent++
	} else {
		r.report.SeenByPoll++
	}
	if status == job.StatusCompleted {
		r.report.Completed++
		r.latency = append(r.latency, seen.Sub(t.sent))
		if seen.After(r.last) {
			r.last = seen
		}
	} else {
		r.report.Dead = append(r.report.Dead, id)
	}
	r.checkAllDone()
}

// checkAllDone closes allDone once every create has returned and every
// created job has finished. Call with mu held.
func (r *run) checkAllDone() {
	if r.closed || !r.created {
		return
	}
	for _, t := range r.jobs {
		if !t.done {
			return
		}
	}
	r.closed = true
	close(r.allDone)
}

// askResync asks for every open job to be checked, without waiting.
func (r *run) askResync() {
	select {
	case r.resync <- struct{}{}:
	default: // one is already pending
	}
}

// unfinished returns up to n IDs not finished yet, oldest create first.
func (r *run) unfinished(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	type idAt struct {
		id string
		at time.Time
	}
	var open []idAt
	for id, t := range r.jobs {
		if !t.done {
			open = append(open, idAt{id, t.sent})
		}
	}
	slices.SortFunc(open, func(a, b idAt) int { return a.at.Compare(b.at) })
	var ids []string
	for i := 0; i < len(open) && (n <= 0 || i < n); i++ {
		ids = append(ids, open[i].id)
	}
	return ids
}

// Run creates cfg.Jobs jobs and follows them. It returns an error only if
// it couldn't run at all (a refused API, a bad token); a run where jobs
// failed or didn't finish returns a Report that says so.
func Run(ctx context.Context, cfg Config) (Report, error) {
	if err := CheckLocal(cfg.API); err != nil {
		return Report{}, err
	}
	if cfg.Jobs < 1 || cfg.Concurrency < 1 {
		return Report{}, errors.New("loadgen: Jobs and Concurrency must be at least 1")
	}
	if cfg.Title == nil {
		cfg.Title = func(int) string { return "sleep:0s" }
	}
	if cfg.PollEvery <= 0 {
		cfg.PollEvery = time.Second
	}
	if cfg.PollBatch <= 0 {
		cfg.PollBatch = 50
	}
	api := strings.TrimRight(cfg.API, "/")
	c := client.New(api, cfg.Token)

	r := &run{jobs: map[string]*tracked{}, early: map[string]seenEarly{}, allDone: make(chan struct{}), resync: make(chan struct{}, 1)}
	r.report.Requested = cfg.Jobs

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Events first, so no job's completion happens before we listen: the
	// server says "ready" once its subscription is live. The stream is a
	// fast path, not a requirement: if it fails or isn't ready within 5 s,
	// the run goes ahead and the polls find every job, only later.
	events, ready := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(events)
		followEvents(ctx, api, cfg.Token, r, ready)
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
	}

	start := time.Now()
	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup
	for i := range cfg.Jobs {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			sent := time.Now()
			createCtx, cancelCreate := context.WithTimeout(ctx, 30*time.Second)
			j, err := c.Create(createCtx, cfg.Title(i))
			cancelCreate()
			r.mu.Lock()
			defer r.mu.Unlock()
			if err != nil {
				r.report.CreateErrors = append(r.report.CreateErrors, fmt.Sprintf("job %d: %v", i, err))
				return
			}
			r.report.Created++
			r.jobs[j.ID] = &tracked{sent: sent}
			if e, ok := r.early[j.ID]; ok {
				delete(r.early, j.ID)
				r.finishLocked(j.ID, e.status, e.seen, e.byEvent)
			}
		}()
	}
	wg.Wait()
	createsTook := time.Since(start)
	r.mu.Lock()
	r.report.CreatesTook = createsTook
	if createsTook > 0 {
		r.report.CreateRate = float64(r.report.Created) / createsTook.Seconds()
	}
	r.created = true
	r.checkAllDone()
	r.mu.Unlock()

	// Wait for every job to finish, asking the API about the oldest
	// unfinished ones each round, until cfg.Wait runs out.
	deadline := time.NewTimer(cfg.Wait)
	defer deadline.Stop()
	poll := time.NewTicker(cfg.PollEvery)
	defer poll.Stop()
waiting:
	for {
		select {
		case <-r.allDone:
			break waiting
		case <-ctx.Done():
			break waiting
		case <-deadline.C:
			// One last authoritative look at everything still open.
			pollJobs(ctx, c, r, r.unfinished(0))
			break waiting
		case <-poll.C:
			pollJobs(ctx, c, r, r.unfinished(cfg.PollBatch))
		case <-r.resync:
			pollJobs(ctx, c, r, r.unfinished(0))
		}
	}
	cancel()
	<-events

	r.mu.Lock()
	defer r.mu.Unlock()
	// Whatever is still open is reported, by ID: a job that never finished
	// must show up in the result, not silently drop out of it.
	for id, t := range r.jobs {
		if !t.done {
			r.report.Unfinished = append(r.report.Unfinished, id)
		}
	}
	slices.Sort(r.report.Unfinished)
	for id := range r.jobs {
		r.report.JobIDs = append(r.report.JobIDs, id)
	}
	if !r.last.IsZero() {
		r.report.Elapsed = r.last.Sub(start)
		r.report.PerSecond = float64(r.report.Completed) / r.report.Elapsed.Seconds()
	}
	r.report.P50, r.report.P95, r.report.P99 = percentile(r.latency, 50), percentile(r.latency, 95), percentile(r.latency, 99)
	return r.report, nil
}

// pollJobs asks the API for each job's status: the authoritative check.
func pollJobs(ctx context.Context, c *client.Client, r *run, ids []string) {
	for _, id := range ids {
		getCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		j, err := c.Get(getCtx, id)
		cancel()
		if err == nil {
			r.finish(id, j.Status, time.Now(), false)
		}
	}
}

// followEvents reads the API's event stream and finishes jobs as their
// "changed" events say. It reconnects with a new ticket if the stream
// drops, and gives up quietly when ctx ends.
func followEvents(ctx context.Context, api, token string, r *run, ready chan<- struct{}) {
	var once sync.Once
	markReady := func() { once.Do(func() { close(ready) }) }
	for ctx.Err() == nil {
		// The server drops a stream that falls 64 messages behind, and a
		// load run sends far more than that: reconnect at once, and have
		// everything open checked, since what was sent meanwhile is gone.
		err := readStream(ctx, api, token, r, markReady)
		if ctx.Err() != nil {
			return
		}
		markReady() // no stream to wait for: the creates go ahead, the polls cover
		r.askResync()
		if err != nil {
			select {
			case <-ctx.Done():
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
}

func readStream(ctx context.Context, api, token string, r *run, markReady func()) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, api+"/api/v1/events/ticket", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	var t struct {
		Ticket string `json:"ticket"`
	}
	err = json.NewDecoder(resp.Body).Decode(&t)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || err != nil {
		return fmt.Errorf("loadgen: events ticket: HTTP %d", resp.StatusCode)
	}

	req, err = http.NewRequestWithContext(ctx, http.MethodGet, api+"/api/v1/events?ticket="+url.QueryEscape(t.Ticket), nil)
	if err != nil {
		return err
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("loadgen: events: HTTP %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var m struct {
			Type string   `json:"type"`
			Job  *job.Job `json:"job"`
		}
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		switch {
		case m.Type == "ready":
			markReady()
		case m.Type == "resync" || m.Type == "resumed":
			r.askResync() // the server says messages were lost
		case m.Type == "changed" && m.Job != nil:
			r.finish(m.Job.ID, m.Job.Status, time.Now(), true)
		}
	}
	return sc.Err()
}

// percentile returns the p-th percentile of ds (nearest rank), or 0 for none.
func percentile(ds []time.Duration, p int) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := slices.Clone(ds)
	slices.Sort(s)
	rank := (p*len(s) + 99) / 100 // ceil(p/100 * n)
	if rank < 1 {
		rank = 1
	}
	return s[rank-1]
}
