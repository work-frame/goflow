package worker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/work-frame/goflow/internal/job"
)

// Handlers returns the Handler both programs run: each job type with its
// own handler. Only demo jobs have one so far; a webhook job is refused by
// Route until Task 65 adds delivery.
func Handlers(demoWork time.Duration) Handler {
	return Route(map[job.Type]Handler{
		job.TypeDemo: Demo(demoWork),
	})
}

// maxDemoSleep caps "sleep:" titles, so one job can't hold a worker for long.
const maxDemoSleep = time.Minute

// Demo returns a Handler whose behaviour is chosen by the job's title. It
// exists to watch retries, timeouts and dead jobs happen, locally and on
// Render, until GoFlow has real job types with their own payloads:
//
//	sleep:3s        works for 3 s (at most 1 minute), then succeeds
//	fail            fails every time: retried, then dead
//	fail:permanent  fails with a permanent error: dead at once
//	flaky:2         fails on attempts 1 and 2, succeeds on attempt 3
//	hang            waits until its deadline (Pool.JobTimeout): timed out
//	anything else   works for `work`, then succeeds
//
// Every case respects ctx, as a real handler must.
func Demo(work time.Duration) Handler {
	return func(ctx context.Context, run Run) error {
		kind, arg, _ := strings.Cut(strings.TrimSpace(run.Title), ":")
		switch strings.ToLower(kind) {
		case "sleep":
			d, err := time.ParseDuration(arg)
			if err != nil || d < 0 {
				return Permanent(fmt.Errorf("demo: %q is not a duration like 3s", arg))
			}
			return sleepOrCancel(ctx, min(d, maxDemoSleep))
		case "fail":
			if arg == "permanent" {
				return Permanent(errors.New("demo: permanent failure"))
			}
			return fmt.Errorf("demo: failure on attempt %d", run.Attempt)
		case "flaky":
			n, err := strconv.Atoi(arg)
			if err != nil || n < 0 {
				return Permanent(fmt.Errorf("demo: %q is not a number of failures", arg))
			}
			if run.Attempt <= n {
				return fmt.Errorf("demo: flaky failure %d of %d", run.Attempt, n)
			}
			return sleepOrCancel(ctx, time.Second)
		case "hang":
			<-ctx.Done()
			return ctx.Err()
		default:
			return sleepOrCancel(ctx, work)
		}
	}
}
