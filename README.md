# GoFlow

GoFlow is a job-processing platform written in Go. You submit background jobs, and GoFlow tracks them through their lifecycle (`pending`, `queued`, `processing`, `completed`, `failed`).

I'm building it to learn production Go: the standard library first, then HTTP, PostgreSQL, Redis queues, concurrent workers, observability, Docker and CI/CD.

## Status

**Phase 1 (complete):** a command-line tool that creates, lists, gets and deletes jobs, stored in a local JSON file.

Next up is Phase 2, which adds an HTTP API using `net/http`.

## Requirements

- Go 1.27 or newer

## Build

```bash
git clone git@github.com:work-frame/goflow.git
cd goflow
go build -o goflow ./cmd/goflow
```

To install it on your `PATH` instead:

```bash
go install ./cmd/goflow
```

## Usage

```bash
goflow job create send welcome email     # prints the new job's ID
goflow job list                          # all jobs, oldest first
goflow job list --status pending         # filter by status
goflow job get <id>                      # one job as JSON
goflow job delete <id>                   # remove a job
```

`create` prints only the ID, so you can capture it in a script:

```bash
id=$(goflow job create generate monthly report)
goflow job get "$id"
```

### Exit codes

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | The command failed (for example, job not found) |
| 2 | The command was called incorrectly |

### Where jobs are stored

Jobs are saved to `~/.goflow/jobs.json`. To use a different file, set `GOFLOW_DATA_FILE`:

```bash
GOFLOW_DATA_FILE=/tmp/test-jobs.json goflow job list
```

Writes go to a temporary file first and are then renamed into place, so a crash never leaves a half-written file.

## Development

Run every check before committing:

```bash
test -z "$(gofmt -l .)" && go vet ./... && go test -count=1 ./...
```

Run the tests with the race detector:

```bash
go test -race ./...
```

## Project layout

```text
cmd/goflow/      entry point; reads the environment and calls internal/cli
internal/cli/    command parsing and output; testable without a real terminal
internal/job/    Job type, validation, in-memory store, JSON persistence
```

## Roadmap

1. CLI and Go fundamentals (done)
2. HTTP API
3. PostgreSQL
4. Authentication
5. Redis job queue
6. Worker pool
7. Retries, backoff, dead letter queue
8. WebSocket status updates
9. Rate limiting
10. Metrics and dashboards
11. Docker
12. Distributed workers
13. Integration and load testing
14. CI/CD