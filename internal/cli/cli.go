package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/work-frame/goflow/internal/client"
	"github.com/work-frame/goflow/internal/job"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// commandTimeout bounds each command, so an unresponsive server can't hang the
// terminal forever. It is long enough for a free hosting plan to wake a
// sleeping server (about a minute on Render's free tier). Ctrl+C still stops
// a command at any time.
const commandTimeout = 90 * time.Second

const usage = `Usage:
  goflow login
  goflow logout
  goflow job create [--max-attempts N] <title>
  goflow job create --webhook <https URL> --body @<file.json> [--max-attempts N] <title>
  goflow job create --webhook <https URL> --body '<JSON>' [--max-attempts N] <title>
  goflow job list [--status <status>]
  goflow job get <id>
  goflow job attempts <id>   a webhook job's delivery attempts
  goflow job delete <id>
  goflow job retry <id>      re-run a dead job with fresh attempts
  goflow webhook-secret      create your webhook signing secret, or replace it

Options go before the title. --body @file reads the body from a file: use it
for anything sensitive, since text typed after --body stays in your shell history.

The CLI talks to a running GoFlow API. Set GOFLOW_API_URL (default http://localhost:8080).
Log in once with "goflow login"; the session is saved in ~/.config/goflow/credentials.
GOFLOW_TOKEN, if set, is used instead (for scripts).
`

// Jobs is what the CLI needs from GoFlow. client.Client satisfies it. Tests use a fake.
type Jobs interface {
	CreateJob(ctx context.Context, n client.NewJob) (*job.Job, error)
	Attempts(ctx context.Context, id string) ([]job.Attempt, error)
	RotateWebhookSecret(ctx context.Context) (secret string, previousUntil *time.Time, err error)
	Get(ctx context.Context, id string) (*job.Job, error)
	List(ctx context.Context, status job.Status, cursor string) ([]*job.Job, string, error)
	Delete(ctx context.Context, id string) error
	Retry(ctx context.Context, id string) (*job.Job, error)
}

// usageError marks a mistake in how the command was called,
// as opposed to a failure while running it.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// Run executes one CLI command and returns the process exit code.
// It never calls os.Exit, so tests can call it directly.
func Run(ctx context.Context, args []string, jobs Jobs, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	var err error
	switch {
	case len(args) >= 1 && args[0] == "webhook-secret":
		err = runWebhookSecret(ctx, jobs, args[1:], stdout, stderr)
	case len(args) >= 2 && args[0] == "job":
		err = runJob(ctx, jobs, args[1], args[2:], stdout, stderr)
	default:
		fmt.Fprint(stderr, usage)
		return exitUsage
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
		if errors.Is(err, client.ErrUnauthenticated) {
			fmt.Fprintln(stderr, "run: goflow login")
		}
		return exitError
	}
}

func runJob(ctx context.Context, jobs Jobs, cmd string, rest []string, stdout, stderr io.Writer) error {
	var err error
	switch cmd {
	case "create":
		err = runCreate(ctx, jobs, rest, stdout, stderr)
	case "attempts":
		err = runAttempts(ctx, jobs, rest, stdout)
	case "list":
		err = runList(ctx, jobs, rest, stdout)
	case "get":
		err = runGet(ctx, jobs, rest, stdout)
	case "delete":
		err = runDelete(ctx, jobs, rest, stdout)
	case "retry":
		err = runRetry(ctx, jobs, rest, stdout)
	default:
		err = usageError{fmt.Sprintf("unknown command %q", cmd)}
	}
	return err
}

func runCreate(ctx context.Context, jobs Jobs, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("create", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	webhookURL := flags.String("webhook", "", "POST the body to this https URL")
	body := flags.String("body", "", "the webhook's JSON body, or @file to read it from a file")
	maxAttempts := flags.Int("max-attempts", 0, "attempts before the job is dead, 1 to 10")
	if err := flags.Parse(args); err != nil {
		return usageError{fmt.Sprintf("create: %v", err)}
	}
	if flags.NArg() == 0 {
		return usageError{"create: missing title (options go before it)"}
	}
	n := client.NewJob{Title: strings.Join(flags.Args(), " "), MaxAttempts: *maxAttempts}

	switch {
	case *webhookURL == "" && *body != "":
		return usageError{"create: --body needs --webhook"}
	case *webhookURL != "" && *body == "":
		return usageError{"create: --webhook needs --body (a JSON value, or @file)"}
	case *webhookURL != "":
		raw, fromFile, err := readBody(*body)
		if err != nil {
			return fmt.Errorf("create: %w", err)
		}
		if !fromFile {
			fmt.Fprintln(stderr, "note: a --body typed on the command line stays in your shell history; use --body @file for anything sensitive")
		}
		n.Type = job.TypeWebhook
		n.Webhook = &job.Webhook{URL: *webhookURL, Body: raw}
	}

	j, err := jobs.CreateJob(ctx, n)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	fmt.Fprintln(stdout, j.ID)
	return nil
}

// readBody returns --body's JSON: the text itself, or a file's contents
// for "@path". It checks that it is JSON before anything is sent.
func readBody(arg string) (raw json.RawMessage, fromFile bool, err error) {
	b := []byte(arg)
	if path, ok := strings.CutPrefix(arg, "@"); ok {
		fromFile = true
		info, err := os.Stat(path)
		if err != nil {
			return nil, true, fmt.Errorf("--body: %w", err)
		}
		if info.Size() > job.MaxWebhookBodyBytes {
			return nil, true, fmt.Errorf("--body: %s is %d bytes, more than %d", path, info.Size(), job.MaxWebhookBodyBytes)
		}
		if b, err = os.ReadFile(path); err != nil {
			return nil, true, fmt.Errorf("--body: %w", err)
		}
	}
	if !json.Valid(b) {
		return nil, fromFile, errors.New("--body is not JSON (a string needs quotes: '\"text\"')")
	}
	return json.RawMessage(b), fromFile, nil
}

func runAttempts(ctx context.Context, jobs Jobs, args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return usageError{"attempts: expected exactly one job ID"}
	}
	list, err := jobs.Attempts(ctx, args[0])
	if err != nil {
		return fmt.Errorf("attempts %s: %w", args[0], err)
	}
	if len(list) == 0 {
		fmt.Fprintln(stdout, "no attempts yet")
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ATTEMPT\tSTARTED\tTOOK\tSTATUS\tOUTCOME\tERROR")
	for _, a := range list {
		status := "-"
		if a.StatusCode != 0 {
			status = fmt.Sprint(a.StatusCode)
		}
		fmt.Fprintf(tw, "%d\t%s\t%dms\t%s\t%s\t%s\n", a.Attempt, a.StartedAt.Local().Format(time.DateTime),
			a.DurationMS, status, a.Outcome, a.Error)
	}
	return tw.Flush()
}

// runWebhookSecret creates or replaces the signing secret and prints it,
// alone on stdout so a script can capture it. GoFlow can't show it again:
// it is never saved by the CLI, and no endpoint returns an existing one.
func runWebhookSecret(ctx context.Context, jobs Jobs, args []string, stdout, stderr io.Writer) error {
	if len(args) != 0 {
		return usageError{"webhook-secret: takes no arguments"}
	}
	secret, previousUntil, err := jobs.RotateWebhookSecret(ctx)
	if err != nil {
		return fmt.Errorf("webhook-secret: %w", err)
	}
	fmt.Fprintln(stdout, secret)
	fmt.Fprintln(stderr, "This is the only time this secret is shown. Put it in your receiver's configuration now.")
	if previousUntil != nil {
		fmt.Fprintf(stderr, "Your previous secret keeps signing deliveries, next to this one, until %s.\n",
			previousUntil.Local().Format(time.DateTime))
	}
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
	fmt.Fprintln(tw, "ID\tSTATUS\tTRIES\tCREATED\tTITLE")

	// The API returns one page at a time. Follow next cursors until the last page.
	cursor := ""
	for {
		page, next, err := jobs.List(ctx, want, cursor)
		if err != nil {
			return fmt.Errorf("list: %w", err)
		}
		for _, j := range page {
			created := j.CreatedAt.Local().Format(time.DateTime)
			fmt.Fprintf(tw, "%s\t%s\t%d/%d\t%s\t%s\n", j.ID, j.Status, j.Attempts, j.MaxAttempts, created, j.Title)
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

func runRetry(ctx context.Context, jobs Jobs, args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return usageError{"retry: expected exactly one job ID"}
	}

	j, err := jobs.Retry(ctx, args[0])
	if err != nil {
		return fmt.Errorf("retry %s: %w", args[0], err)
	}
	fmt.Fprintf(stdout, "%s is %s again (attempts reset to %d of %d)\n", j.ID, j.Status, j.Attempts, j.MaxAttempts)
	return nil
}
