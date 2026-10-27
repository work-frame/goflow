# GoFlow

GoFlow is a job-processing platform written in Go. You submit background jobs, and GoFlow tracks them through their lifecycle (`pending`, `queued`, `processing`, `completed`, `failed`).

I'm building it to learn production Go: the standard library first, then HTTP, PostgreSQL, Redis queues, concurrent workers, observability, Docker and CI/CD.

## Status

- **Phase 1 (done):** command-line tool backed by a local JSON file
- **Phase 2 (done):** HTTP API with request IDs, structured logs, panic recovery and graceful shutdown. Jobs are kept in memory and lost on restart.
- **Phase 3 (next):** PostgreSQL storage

## Requirements

- Go 1.27 or newer

## Build

```bash
git clone git@github.com:work-frame/goflow.git
cd goflow
go build -o bin/goflow ./cmd/goflow   # CLI
go build -o bin/api ./cmd/api         # HTTP API
```

## HTTP API

### Run

```bash
./bin/api
```

| Variable | Default | Meaning |
|---|---|---|
| `GOFLOW_ADDR` | `:8080` | Address to listen on. Use `:0` for any free port. |
| `GOFLOW_BCRYPT_COST` | `12` | bcrypt cost for password hashes, 10 to 14. Each step doubles the time per hash. The server refuses to start outside this range. |

The server stops on `Ctrl+C` or `SIGTERM`, finishing in-flight requests first (up to 10 seconds).

### Endpoints

| Method | Path | Success | Errors |
|---|---|---|---|
| `GET` | `/healthz` | `200 {"status":"ok"}` | |
| `POST` | `/api/v1/auth/register` | `201` + user (`id`, `email`, `created_at`) | `400`, `409` email taken, `413`, `415` |
| `POST` | `/api/v1/auth/login` | `200 {"token","expires_at"}` | `400`, `401` invalid email or password |
| `POST` | `/api/v1/auth/logout` | `204` (needs token) | `401` |
| `POST` | `/api/v1/auth/logout-all` | `200 {"sessions_ended":n}`, every device including this one (needs token) | `401` |
| `POST` | `/api/v1/auth/password` | `200 {"sessions_ended":n}`, ends your other sessions (needs token) | `400`, `401`, `403` wrong current password |
| `GET` | `/api/v1/auth/me` | `200` + user (needs token) | `401` |
| `POST` | `/api/v1/jobs` | `201` + job, `Location` header (needs token) | `400`, `401`, `413`, `415` |
| `GET` | `/api/v1/jobs` | `200 {"jobs":[...]}`, your jobs only (needs token) | `400` invalid `status` filter, `401` |
| `GET` | `/api/v1/jobs/{id}` | `200` + job (needs token) | `401`, `404` |
| `DELETE` | `/api/v1/jobs/{id}` | `204`, no body (needs token) | `401`, `404` |

`GET /api/v1/jobs` accepts an optional `?status=` filter: `pending`, `queued`, `processing`, `completed` or `failed`.

Every job belongs to the user who created it. Users only ever see their own jobs: someone else's job answers `404`, exactly like a job that doesn't exist.

### Examples

Log in once and keep the token in an environment variable. `read -s` asks for the password without showing it, so neither the password nor the token ends up in your shell history (in bash, use `read -s -p "Password: " PW`):

```bash
API=http://localhost:8080/api/v1
read -s "PW?Password: " && echo
export GOFLOW_TOKEN=$(jq -n --arg e dan@example.com --arg p "$PW" '{email:$e,password:$p}' \
  | curl -s -X POST $API/auth/login -H 'Content-Type: application/json' -d @- | jq -r .token)
unset PW

# gf sends the token on stdin (-H @-), so it never appears in the process list.
gf() { printf 'Authorization: Bearer %s\n' "$GOFLOW_TOKEN" | curl -s -H @- "$@"; }
gf -X POST $API/jobs -H 'Content-Type: application/json' -d '{"title":"send welcome email"}'
gf $API/jobs
gf "$API/jobs?status=pending"
gf $API/jobs/<id>
gf -X DELETE $API/jobs/<id>
gf -X POST $API/auth/logout     # ends the session at once
```

### Request rules

- `POST` bodies must be sent with `Content-Type: application/json`.
- The body must be a single JSON object, at most 1 MiB.
- Unknown fields are rejected, not ignored.
- `title` is required, at most 200 characters.
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

The CLI is a client for the HTTP API, so the API must be running.

```bash
goflow job create send welcome email     # prints the new job's ID
goflow job list                          # every job, oldest first (follows pages)
goflow job list --status pending         # filter by status
goflow job get <id>                      # one job as JSON
goflow job delete <id>                   # remove a job
```

| Variable | Default | Meaning |
|---|---|---|
| `GOFLOW_API_URL` | `http://localhost:8080` | Where the API is running |
| `GOFLOW_TOKEN` | none | Session token from login (see the examples above). A stopgap until `goflow login` |

Exit codes: `0` success, `1` failure (including an unreachable API), `2` incorrect usage. Each command gives up after 10 seconds.

## Development

Run every check before committing:

```bash
./scripts/check.sh
```

It runs `gofmt`, `go vet` and `go test -race`, and stops at the first failure.

## Project layout

```text
cmd/goflow/      CLI entry point
cmd/api/         HTTP server: startup, timeouts, graceful shutdown
internal/api/    routes, handlers, middleware, JSON helpers
internal/cli/    CLI command parsing and output
internal/job/    Job type, validation, concurrent-safe store, JSON persistence
scripts/         development scripts
```

## Roadmap

1. CLI and Go fundamentals (done)
2. HTTP API (done)
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


See [FAILURES.md](FAILURES.md) for what breaks when each safeguard is removed, with real output.

`GET /api/v1/jobs` returns jobs oldest first, one page at a time:

| Parameter | Default | Meaning |
|---|---|---|
| `status` | (all) | `pending`, `queued`, `processing`, `completed` or `failed` |
| `limit` | 50 | Jobs per page, 1 to 200 |
| `cursor` | (start) | The `next_cursor` from the previous page. Treat it as opaque |

The response includes `next_cursor` until the last page, where it is omitted.
