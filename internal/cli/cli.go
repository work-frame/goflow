package cli

import (
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

const usage = `Usage:
  goflow job create <title>
  goflow job list [--status <status>]
  goflow job get <id>
  goflow job delete <id>
`

// usageError marks a mistake in how the command was called,
// as opposed to a failure while running it.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// Run executes one CLI command and returns the process exit code.
// It never calls os.Exit, so tests can call it directly.
func Run(args []string, dataPath string, stdout, stderr io.Writer) int {
	if len(args) < 2 || args[0] != "job" {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}

	store, err := job.Load(dataPath)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitError
	}

	cmd, rest := args[1], args[2:]
	switch cmd {
	case "create":
		err = runCreate(store, dataPath, rest, stdout)
	case "list":
		err = runList(store, rest, stdout)
	case "get":
		err = runGet(store, rest, stdout)
	case "delete":
		err = runDelete(store, dataPath, rest, stdout)
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

func runCreate(store *job.MemoryStore, dataPath string, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return usageError{"create: missing title"}
	}

	j, err := job.New(strings.Join(args, " "))
	if err != nil {
		return err
	}
	if err := store.Add(j); err != nil {
		return err
	}
	if err := store.Save(dataPath); err != nil {
		return err
	}

	fmt.Fprintln(stdout, j.ID)
	return nil
}

func runList(store *job.MemoryStore, args []string, stdout io.Writer) error {
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
	for _, j := range store.List() {
		if want != "" && j.Status != want {
			continue
		}
		created := j.CreatedAt.Local().Format(time.DateTime)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", j.ID, j.Status, created, j.Title)
	}
	return tw.Flush()
}

func runGet(store *job.MemoryStore, args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return usageError{"get: expected exactly one job ID"}
	}

	j, err := store.Get(args[0])
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

func runDelete(store *job.MemoryStore, dataPath string, args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return usageError{"delete: expected exactly one job ID"}
	}

	id := args[0]
	if err := store.Delete(id); err != nil {
		return fmt.Errorf("delete %s: %w", id, err)
	}
	if err := store.Save(dataPath); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "deleted %s\n", id)
	return nil
}
