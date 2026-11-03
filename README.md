# GoFlow

GoFlow is a job-processing platform written in Go. You submit background jobs, and GoFlow tracks them through their lifecycle (`pending`, `queued`, `processing`, then `completed`, or `retrying` and finally `dead` when a job keeps failing).

I'm building it to learn production Go: the standard library first, then HTTP, PostgreSQL, Redis queues, concurrent workers, observability, Docker and CI/CD.

## Status

- **Phase 1 (done):** command-line tool backed by a local JSON file
- **Phase 2 (done):** HTTP API with request IDs, structured logs, panic recovery and graceful shutdown
- **Phase 3 (done):** PostgreSQL storage with migrations, constraints and keyset pagination
- **Phase 4 (done):** accounts, login sessions, per-user jobs, `goflow login`, and a first deployment on Render and Neon ([DEPLOYMENT.md](DEPLOYMENT.md))
- **Phase 5 (done):** Redis job queue. A new job and an outbox row are saved in one transaction; with `GOFLOW_REDIS_URL` set, a background relay moves the ID to a Redis list and marks the job `queued`. A single worker (`cmd/worker`) takes IDs off the queue and runs jobs to `completed`; the work itself is a placeholder until real job types arrive. Runs locally; not deployed yet.
- **Phase 6 (done):** a pool of concurrent workers (`GOFLOW_WORKERS`) with graceful shutdown. On Render, the API runs the pool itself against a free Key Value queue ([DEPLOYMENT.md](DEPLOYMENT.md)). Jobs are created, queued and completed end to end in production; the work is still a 5-second placeholder.
- **Phase 7 (done):** failed jobs retry with exponential backoff and jitter, up to 3 attempts, then become `dead`; permanent errors skip the retries. Each attempt has a 15-second limit. A dead job can be re-run with fresh attempts (`goflow job retry <id>`), and an `Idempotency-Key` header makes a repeated create return the first job instead of a second one.

## Requirements

- Go 1.27 or newer

## Build

```bash
git clone git@github.com:work-frame/goflow.git
cd goflow
go build -o bin/goflow ./cmd/goflow   # CLI
go build -o bin/api ./cmd/api         # HTTP API
go build -o bin/worker ./cmd/worker   # job worker
```

## HTTP API

### Run

```bash
./bin/api
```

| Variable | Default | Meaning |
|---|---|---|
| `GOFLOW_DATABASE_URL` | none (required) | PostgreSQL URL. A database on another machine must use `sslmode=verify-full` (or `require`); the server refuses to start otherwise. |
| `GOFLOW_ADDR` | `:8080` | Address to listen on. Use `:0` for any free port. |
| `GOFLOW_BCRYPT_COST` | `12` | bcrypt cost for password hashes, 10 to 14. Each step doubles the time per hash. The server refuses to start outside this range. |
| `GOFLOW_REGISTRATION` | `closed` | `open` lets anyone create an account with `POST /api/v1/auth/register`; `closed` answers `403`. |
| `PORT` | none | Port to listen on, as set by hosting platforms such as Render. Used only when `GOFLOW_ADDR` is not set. |
| `GOFLOW_REDIS_URL` | none (queue off) | Redis for the job queue, e.g. `redis://127.0.0.1:6379/0`. A Redis on another machine must use `rediss://` (TLS), except a single-label private-network name such as Render's `red-abc123`. Without it, new jobs stay `pending` in the outbox until a server with Redis starts. |
| `GOFLOW_WORKERS` | `0` | Job workers the API runs itself, 0 to 32. Needs `GOFLOW_REDIS_URL`. Only one API instance may run workers. |

The server stops on `Ctrl+C` or `SIGTERM`, finishing in-flight requests first (up to 10 seconds).

### Endpoints

| Method | Path | Success | Errors |
|---|---|---|---|
| `GET` | `/healthz` | `200 {"status":"ok"}`: the process is alive | |
| `GET` | `/readyz` | `200`: the database answers within 2 s. Redis isn't checked: jobs wait in the outbox while it's down | `503` |
| `POST` | `/api/v1/auth/register` | `201` + user (`id`, `email`, `created_at`) | `400`, `409` email taken, `413`, `415` |
| `POST` | `/api/v1/auth/login` | `200 {"token","expires_at"}` | `400`, `401` invalid email or password |
| `POST` | `/api/v1/auth/logout` | `204` (needs token) | `401` |
| `POST` | `/api/v1/auth/logout-all` | `200 {"sessions_ended":n}`, every device including this one (needs token) | `401` |
| `POST` | `/api/v1/auth/password` | `200 {"sessions_ended":n}`, ends your other sessions (needs token) | `400`, `401`, `403` wrong current password |
| `GET` | `/api/v1/auth/me` | `200` + user (needs token) | `401` |
| `POST` | `/api/v1/jobs` | `201` + job, `Location` header (needs token). With a repeated `Idempotency-Key`: `200` + the first job and `Idempotent-Replayed: true` | `400`, `401`, `413`, `415`, `422` key reused with a different title |
| `GET` | `/api/v1/jobs` | `200 {"jobs":[...]}`, your jobs only (needs token) | `400` invalid `status` filter, `401` |
| `GET` | `/api/v1/jobs/{id}` | `200` + job (needs token) | `401`, `404` |
| `DELETE` | `/api/v1/jobs/{id}` | `204`, no body (needs token) | `401`, `404` |
| `POST` | `/api/v1/jobs/{id}/retry` | `200` + job, back to `pending` with 0 attempts (needs token) | `401`, `404`, `409` job isn't `dead` or `failed` |

`GET /api/v1/jobs` returns jobs oldest first, one page at a time:

| Parameter | Default | Meaning |
|---|---|---|
| `status` | (all) | `pending`, `queued`, `processing`, `retrying`, `completed`, `dead`, or `failed` (jobs from before retries) |
| `limit` | 50 | Jobs per page, 1 to 200 |
| `cursor` | (start) | The `next_cursor` from the previous page. Treat it as opaque |

The response includes `next_cursor` until the last page, where it is omitted.

Every job belongs to the user who created it. Users only ever see their own jobs: someone else's job answers `404`, exactly like a job that doesn't exist.

### Examples

`read -s` asks for the password without showing it, so neither the password nor the token ends up in your shell history (in bash, use `read -s -p "Password: " PW`). Paste each block separately: a `read` pasted together with the next lines takes those lines as its answer.

```bash
API=http://localhost:8080/api/v1
read "EMAIL?Email: "
```

```bash
read -s "PW?Password: " && echo
```

Create an account (only while the server runs with `GOFLOW_REGISTRATION=open`):

```bash
E="$EMAIL" P="$PW" jq -n '{email:env.E,password:env.P}' \
  | curl -s -X POST $API/auth/register -H 'Content-Type: application/json' -d @-
```

Log in once and keep the token in an environment variable:

```bash
# jq reads the password from its environment (env.P). Passed as an argument
# (--arg p "$PW") it would show in the process list while jq runs.
export GOFLOW_TOKEN=$(E="$EMAIL" P="$PW" jq -n '{email:env.E,password:env.P}' \
  | curl -s -X POST $API/auth/login -H 'Content-Type: application/json' -d @- | jq -r .token)
unset PW

# gf sends the token on stdin (-H @-), so it never appears in the process list.
gf() { printf 'Authorization: Bearer %s\n' "$GOFLOW_TOKEN" | curl -s -H @- "$@"; }
gf -X POST $API/jobs -H 'Content-Type: application/json' -d '{"title":"send welcome email"}'
gf $API/jobs
gf "$API/jobs?status=pending"
gf $API/jobs/<id>
gf -X DELETE $API/jobs/<id>
gf -X POST $API/jobs/<id>/retry   # re-run a dead job
# Safe to send twice: the second answer is the first job, not a new one.
K=$(uuidgen)
gf -X POST $API/jobs -H 'Content-Type: application/json' -H "Idempotency-Key: $K" -d '{"title":"send invoice 42"}'
gf -X POST $API/auth/logout     # ends the session at once
```

### Request rules

- `POST` bodies must be sent with `Content-Type: application/json`.
- The body must be a single JSON object, at most 1 MiB.
- Unknown fields are rejected, not ignored.
- `title` is required, at most 200 characters.
- `Idempotency-Key` (optional, on `POST /api/v1/jobs`) is 1 to 255 visible ASCII characters, sent once; a UUID works. Use a new key for each new job and the same key when you send the same request again after a timeout. A key belongs to your account and lasts as long as the job it created: after you delete that job, the key can create a new one.
- `email` is required and stored lowercased, so `Dan@x.com` and `dan@x.com` are one account.
- `password` must be at least 12 characters and at most 72 bytes. It is stored only as a bcrypt hash and never returned.
- Send the login token as `Authorization: Bearer <token>`. It lasts 24 hours, and logout ends it immediately. The server stores only its SHA-256 hash.
- `POST /api/v1/auth/password` takes `{"current_password": "...", "new_password": "..."}`. It keeps the session that made the change and ends all others. `logout-all` ends every session, this one included. The API deletes expired sessions every hour.

### Errors

Every error response is JSON:

```json
{"error": "job not found"}
```

Unexpected server errors return `500 {"error":"internal error"}`. The details are logged server-side, never sent to the client.

### Request IDs

Every response carries an `X-Request-ID` header, and the same ID appears in the server log line for that request. Send your own `X-Request-ID` (letters, digits, `-`, `_`, up to 64 characters) to trace a request through the logs. Anything else is replaced with a generated ID.

## CLI

The CLI is a client for the HTTP API, so the API must be running, locally or hosted (`export GOFLOW_API_URL=https://goflow-api-hewh.onrender.com`).

```bash
goflow login                             # asks for email and password (the password isn't shown)
goflow job create send welcome email     # prints the new job's ID
goflow job list                          # every job, oldest first, with TRIES (attempts/max)
goflow job list --status pending         # filter by status
goflow job get <id>                      # one job as JSON
goflow job delete <id>                   # remove a job
goflow job retry <id>                    # re-run a dead job with fresh attempts
goflow logout                            # ends the session on the server and deletes the saved login
```

Until GoFlow has real job types, a job's title can pick a test behaviour, to watch retries and timeouts happen:

| Title | What the worker does |
|---|---|
| `sleep:3s` | works for 3 s (at most 1 minute), then succeeds |
| `fail` | fails every time: retried with backoff, then `dead` after 3 attempts |
| `fail:permanent` | fails with a permanent error: `dead` after 1 attempt |
| `flaky:2` | fails on attempts 1 and 2, succeeds on attempt 3 |
| `hang` | never finishes on its own: cancelled at the 15-second limit and retried |
| anything else | works for 5 s, then succeeds |

| Variable | Default | Meaning |
|---|---|---|
| `GOFLOW_API_URL` | `http://localhost:8080` | Where the API is running |
| `GOFLOW_TOKEN` | none | A session token to use instead of the saved login, for scripts and CI |

`goflow login` saves the session in `~/.config/goflow/credentials` (or `$XDG_CONFIG_HOME/goflow/credentials`) with mode `0600`, so only you can read it. The CLI refuses to use the file if other users can read it, and only sends the token to the API that issued it. In a script, pipe the email and password on two lines: `printf '%s\n%s\n' "$EMAIL" "$PASSWORD" | goflow login`.

Exit codes: `0` success, `1` failure (including an unreachable API), `2` incorrect usage. Each command gives up after 90 seconds, long enough for a free hosting plan to wake a sleeping server; Ctrl+C stops it sooner.

## Development

Run every check before committing:

```bash
./scripts/check.sh
```

It runs `gofmt`, `go vet` and `go test -race`, and stops at the first failure.

## Project layout

```text
cmd/goflow/            CLI entry point
cmd/api/               HTTP server: settings, startup, session cleanup, graceful shutdown
internal/api/          routes, handlers, middleware, JSON helpers
internal/auth/         users, password hashing, session tokens
internal/cli/          CLI commands, login prompt, output
internal/client/       Go client for the HTTP API
internal/credentials/  the saved login file (~/.config/goflow/credentials)
internal/database/     PostgreSQL: connection, migrations, stores
internal/job/          Job type, validation, pagination rules
internal/queue/        Redis list of job IDs waiting for a worker
internal/relay/        background loop: outbox (PostgreSQL) to queue (Redis)
internal/worker/       pool of goroutines that take job IDs off the queue and run the jobs
internal/urlcheck/     TLS rules for database and Redis URLs, shared by the API and the worker
cmd/worker/            worker entry point
scripts/               development scripts
render.yaml            Render deployment (see DEPLOYMENT.md)
```

## Roadmap

1. CLI and Go fundamentals (done)
2. HTTP API (done)
3. PostgreSQL (done)
4. Authentication and first deployment (done)
5. Redis job queue (done)
6. Worker pool (done)
7. Retries, backoff, dead letter queue (done)
8. WebSocket status updates
9. Rate limiting
10. Metrics and dashboards
11. Docker
12. Distributed workers
13. Integration and load testing
14. CI/CD

See [FAILURES.md](FAILURES.md) for what breaks when each safeguard is removed, with real output, and [DEPLOYMENT.md](DEPLOYMENT.md) for how the hosted API is set up.
