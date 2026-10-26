package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/work-frame/goflow/internal/job"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// commandTimeout bounds each command, so an unresponsive server can't hang the terminal.
const commandTimeout = 10 * time.Second

const usage = `Usage:
  goflow job create <title>
  goflow job list [--status <status>]
  goflow job get <id>
  goflow job delete <id>

The CLI talks to a running GoFlow API. Set GOFLOW_API_URL (default http://localhost:8080).
`

// Jobs is what the CLI needs from GoFlow. client.Client satisfies it. Tests use a fake.
type Jobs interface {
	Create(ctx context.Context, title string) (*job.Job, error)
	Get(ctx context.Context, id string) (*job.Job, error)
	List(ctx context.Context, status job.Status, cursor string) ([]*job.Job, string, error)
	Delete(ctx context.Context, id string) error
}

// usageError marks a mistake in how the command was called,
// as opposed to a failure while running it.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// Run executes one CLI command and returns the process exit code.
// It never calls os.Exit, so tests can call it directly.
func Run(ctx context.Context, args []string, jobs Jobs, stdout, stderr io.Writer) int {
	if len(args) < 2 || args[0] != "job" {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	cmd, rest := args[1], args[2:]
	var err error
	switch cmd {
	case "create":
		err = runCreate(ctx, jobs, rest, stdout)
	case "list":
		err = runList(ctx, jobs, rest, stdout)
	case "get":
		err = runGet(ctx, jobs, rest, stdout)
	case "delete":
		err = runDelete(ctx, jobs, rest, stdout)
	default:
		err = usageError{fmt.Sprintf("unknown command %q", cmd)}
	}

	var ue usageError
	switch {
	case err == nil:
		return exitOK
	case errors.As(err, &ue):
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return exitUsage
	default:
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitError
	}
}

func runCreate(ctx context.Context, jobs Jobs, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return usageError{"create: missing title"}
	}

	j, err := jobs.Create(ctx, strings.Join(args, " "))
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	fmt.Fprintln(stdout, j.ID)
	return nil
}

func runList(ctx context.Context, jobs Jobs, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("list", flag.ContinueOnError)
	flags.SetOutput(io.Discard) // we print our own usage message
	status := flags.String("status", "", "only show jobs with this status")

	if err := flags.Parse(args); err != nil {
		return usageError{fmt.Sprintf("list: %v", err)}
	}
	if flags.NArg() > 0 {
		return usageError{fmt.Sprintf("list: unexpected argument %q", flags.Arg(0))}
	}
	want := job.Status(*status)
	if want != "" && !want.Valid() {
		return usageError{fmt.Sprintf("list: invalid status %q", *status)}
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tCREATED\tTITLE")

	// The API returns one page at a time. Follow next cursors until the last page.
	cursor := ""
	for {
		page, next, err := jobs.List(ctx, want, cursor)
		if err != nil {
			return fmt.Errorf("list: %w", err)
		}
		for _, j := range page {
			created := j.CreatedAt.Local().Format(time.DateTime)
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", j.ID, j.Status, created, j.Title)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	return tw.Flush()
}

func runGet(ctx context.Context, jobs Jobs, args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return usageError{"get: expected exactly one job ID"}
	}

	j, err := jobs.Get(ctx, args[0])
	if err != nil {
		return fmt.Errorf("get %s: %w", args[0], err)
	}
	out, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return fmt.Errorf("get %s: encode: %w", args[0], err)
	}
	fmt.Fprintln(stdout, string(out))
	return nil
}

func runDelete(ctx context.Context, jobs Jobs, args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return usageError{"delete: expected exactly one job ID"}
	}

	id := args[0]
	if err := jobs.Delete(ctx, id); err != nil {
		return fmt.Errorf("delete %s: %w", id, err)
	}
	fmt.Fprintf(stdout, "deleted %s\n", id)
	return nil
}
