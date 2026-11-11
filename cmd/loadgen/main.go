// Command loadgen measures how fast the GoFlow API on this machine gets jobs
// done (Task 56). It refuses any API that isn't on this machine.
//
//	./bin/goflow login                       # once: loadgen uses the saved login
//	./bin/loadgen -jobs 500 -workers 6        # see scripts/loadtest.sh
//
// It exits 1 when a job failed to be created, ended dead or didn't finish
// in time, so a script can't mistake a partial run for a result.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"time"

	"github.com/work-frame/goflow/internal/client"
	"github.com/work-frame/goflow/internal/credentials"
	"github.com/work-frame/goflow/internal/loadgen"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("loadgen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	api := fs.String("api", envOr("GOFLOW_API_URL", "http://localhost:8080"), "the API on this machine (only localhost or a loopback address)")
	jobs := fs.Int("jobs", 500, "jobs to create")
	concurrency := fs.Int("concurrency", 20, "create requests in flight at once")
	wait := fs.Duration("wait", 2*time.Minute, "how long jobs have to finish after the last create")
	workers := fs.Int("workers", 0, "workers running the jobs, as counted by scripts/loadtest.sh (printed in the report)")
	label := fs.String("label", "", "a name for this run in the report")
	keep := fs.Bool("keep", false, "keep the created jobs instead of deleting them after the report")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// Refuse before reading credentials or sending anything.
	if err := loadgen.CheckLocal(*api); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	path, err := credentials.DefaultPath()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	token, err := credentials.File{Path: path}.TokenFor(*api, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if token == "" {
		fmt.Fprintf(stderr, "loadgen: not logged in to %s: run goflow login first (with GOFLOW_API_URL=%s)\n", *api, *api)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	rep, err := loadgen.Run(ctx, loadgen.Config{API: *api, Token: token, Jobs: *jobs, Concurrency: *concurrency, Wait: *wait})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	printReport(stdout, *label, *workers, rep)
	if !*keep {
		// After the report, so deleting doesn't count as load.
		deleted := deleteJobs(context.Background(), client.New(*api, token), rep.JobIDs)
		fmt.Fprintf(stdout, "cleanup:      deleted %d of %d jobs (-keep to keep them)\n", deleted, len(rep.JobIDs))
	}
	if len(rep.CreateErrors) > 0 || len(rep.Dead) > 0 || len(rep.Unfinished) > 0 {
		return 1
	}
	return 0
}

func printReport(w io.Writer, label string, workers int, r loadgen.Report) {
	if label != "" {
		fmt.Fprintf(w, "run:          %s\n", label)
	}
	if workers > 0 {
		fmt.Fprintf(w, "workers:      %d\n", workers)
	}
	fmt.Fprintf(w, "jobs:         %d requested, %d created, %d completed, %d dead, %d unfinished, %d create errors\n",
		r.Requested, r.Created, r.Completed, len(r.Dead), len(r.Unfinished), len(r.CreateErrors))
	fmt.Fprintf(w, "throughput:   %.1f jobs/s (%d completed in %v, first create to last completion)\n",
		r.PerSecond, r.Completed, r.Elapsed.Round(time.Millisecond))
	fmt.Fprintf(w, "create→done:  p50 %v  p95 %v  p99 %v\n", r.P50.Round(time.Millisecond), r.P95.Round(time.Millisecond), r.P99.Round(time.Millisecond))
	fmt.Fprintf(w, "seen by:      %d events, %d polls\n", r.SeenByEvent, r.SeenByPoll)
	for _, e := range r.CreateErrors {
		fmt.Fprintf(w, "create error: %s\n", e)
	}
	for _, id := range r.Dead {
		fmt.Fprintf(w, "dead:         %s\n", id)
	}
	for _, id := range r.Unfinished {
		fmt.Fprintf(w, "unfinished:   %s\n", id)
	}
}

// deleteJobs deletes the run's jobs, ten at a time, and returns how many
// went.
func deleteJobs(ctx context.Context, c *client.Client, ids []string) int {
	var deleted atomic.Int64
	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup
	for _, id := range ids {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			delCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if c.Delete(delCtx, id) == nil {
				deleted.Add(1)
			}
		}()
	}
	wg.Wait()
	return int(deleted.Load())
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
