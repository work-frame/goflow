# Architecture

This describes GoFlow at the end of Phase 3. It changes as each phase lands.

## Overview

```text
goflow (CLI)                                       api (server)
cmd/goflow ─► internal/cli ─► internal/client ──HTTP──► cmd/api ─► internal/api ─► internal/database ─► PostgreSQL
                                                                         │                    │
                                                                         └─── internal/job ───┘
                                                         (Job, statuses, validation, pagination rules)
```

- **One way in.** The CLI is an HTTP client of the API. Only the API knows the database URL, so every change to a job passes through the same validation, logging and (from Phase 4) authentication.
- **`internal/job`** is the domain: what a job is and the rules for listing jobs. It imports no HTTP or SQL packages.
- **`internal/database`** owns PostgreSQL: connecting, migrating, and storing jobs.
- **`internal/api`** owns HTTP: routes, middleware, handlers.
- **`internal/client`** is a Go client for the API. **`internal/cli`** uses it.
- **`cmd/*`** only wire things together: read environment variables, build dependencies, run, exit.

## Domain: `internal/job`

| Item | Purpose |
|---|---|
| `Job` | `id` (32 hex, `crypto/rand`), `title` (1–200 characters), `status`, `created_at`, `updated_at` (UTC, microsecond precision) |
| `Status` | `pending`, `queued`, `processing`, `completed`, `failed` |
| `New(title)` | The only way to create a valid job |
| `ListParams`, `Page`, `Cursor` | One page of jobs, oldest first by `(created_at, id)` |
| `NewPage`, `Paginate` | The pagination rules, shared by the database store and in-memory fakes |
| `MemoryStore` | An in-memory store, used as a fast stand-in in tests |

## Storage: `internal/database`

- **`Open`** creates a `database/sql` pool using the `pgx` driver (10 open, 10 idle, 5-minute idle limit, 30-minute lifetime) and pings within 5 seconds.
- **`Migrate`** applies the SQL files embedded from `migrations/`, each in its own transaction together with its row in `schema_migrations`, while holding a PostgreSQL advisory lock. Several API instances can start at once safely.
- **`JobStore`** implements `Add`, `Get`, `List` and `Delete`. Callers only ever see `job` errors: a duplicate key becomes `ErrDuplicateID`, and a missing row becomes `ErrNotFound`.

### Schema

```sql
jobs (
  id         TEXT        PRIMARY KEY  -- 32 lowercase hex characters
  title      TEXT        NOT NULL     -- 1 to 200 characters
  status     TEXT        NOT NULL     -- one of the 5 statuses
  created_at TIMESTAMPTZ NOT NULL
  updated_at TIMESTAMPTZ NOT NULL     -- never before created_at
)
INDEX (created_at, id)       -- listing and pagination
INDEX (status, created_at)   -- status filter; workers in Phase 6
```

Every rule is also a `CHECK` constraint, so bad data is rejected even if it doesn't come from GoFlow.

## HTTP API: `internal/api` and `cmd/api`

### Request flow

```text
client → withRequestID → logRequests → recoverPanics → jsonMuxErrors → ServeMux → handler → JobStore
```

Every storage call gets `context.WithTimeout(r.Context(), 3s)`: it stops when the client disconnects or after 3 seconds, whichever comes first.

### Endpoints

| Method | Path | Success |
|---|---|---|
| `GET` | `/healthz` | `200`: the process is alive (checks nothing else) |
| `GET` | `/readyz` | `200`: the database answers a ping within 2 s, else `503` |
| `POST` | `/api/v1/jobs` | `201` + job + `Location` |
| `GET` | `/api/v1/jobs?status=&limit=&cursor=` | `200` + `{"jobs": [...], "next_cursor": "..."}` |
| `GET` | `/api/v1/jobs/{id}` | `200` + job |
| `DELETE` | `/api/v1/jobs/{id}` | `204` |

Listing uses **keyset pagination**: `WHERE (created_at, id) > (cursor)`, `LIMIT limit + 1`. The extra row shows whether another page exists. Cursors are opaque base64url strings, validated on every request.

### Errors

All errors are `{"error": "..."}`.

| Status | When |
|---|---|
| 400 | Invalid body, title, status, limit or cursor |
| 404 / 405 | Unknown job or route / wrong method |
| 413 / 415 | Body over 1 MiB / not `application/json` |
| 499 | Client disconnected (logged at Info; not a server error) |
| 503 | Storage timed out |
| 500 | Anything else. The cause is logged with the request ID, never sent to the client |

### Lifecycle

1. Require `GOFLOW_DATABASE_URL`, connect, run migrations. No port is opened if this fails.
2. Bind the port, log `listening`, serve.
3. On `SIGINT`/`SIGTERM`: stop accepting connections, finish in-flight requests (up to 10 s), then close the database pool.

## CLI: `internal/cli`, `internal/client`, `cmd/goflow`

- `goflow job create | list [--status] | get | delete` sends HTTP requests to `GOFLOW_API_URL` (default `http://localhost:8080`).
- Each command has a 10-second deadline, and `Ctrl+C` cancels the request in flight.
- `list` follows `next_cursor` until the last page.
- The client turns a 404 `job not found` back into `job.ErrNotFound`, reports other non-success responses as `*client.APIError`, and reports network failures as ordinary wrapped errors.

## Testing

| Layer | Approach |
|---|---|
| `internal/job` | Plain unit tests, including the pagination rules |
| `internal/api` | Handlers with fakes: an adapter over `MemoryStore`, an always-failing store, and a spy that records the context it was given |
| `internal/cli` | A fake API with 2 jobs per page, plus failure and deadline doubles |
| `internal/client` | **Contract tests** against the real `api.NewRouter`, served by `httptest.NewServer` |
| `internal/database` | Real PostgreSQL. `TestMain` migrates one template database per run, and each test copies it |
| `cmd/api` | End to end: the real server on `127.0.0.1:0`, including jobs surviving a restart |

Everything runs with the race detector (`./scripts/check.sh`). [FAILURES.md](FAILURES.md) records what each safeguard prevents.

## Design decisions

| Decision | Reason |
|---|---|
| Standard library first; `pgx` is the only dependency | Learn what frameworks hide |
| CLI goes through the API | One set of rules; no back door around authentication |
| Interfaces defined by the code that uses them, only once a second implementation exists | `JobStore` and `Pinger` each have a real and a test implementation |
| Keyset pagination with opaque cursors | Fast at any depth; correct when jobs are added or removed between pages |
| Constraints in the database as well as in Go | The database outlives any one program |
| Migrations at startup under an advisory lock | Schema always matches the code; safe with several instances |
| Liveness never checks dependencies | A database outage shouldn't restart every API instance |
| Request context carried into every query | A disconnected client stops its query |
| Tests never use the development database | Tests can't damage dev data, or depend on it |

## Known limitations

- No authentication: anyone who can reach the API can create and delete any job (Phase 4).
- Jobs are stored but never run: there is no queue (Phase 5) and no workers (Phase 6).
- A failed readiness check is logged twice (Phase 10).