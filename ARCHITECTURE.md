# Architecture

This describes GoFlow as it is today (end of Phase 2). It changes as each phase lands.

## Overview

GoFlow has two programs that share one domain package:

```text
cmd/goflow  (CLI) ──► internal/cli ──┐
                                     ├──► internal/job  (Job, validation, store, file persistence)
cmd/api     (HTTP) ──► internal/api ─┘
```

- **`internal/job`** knows nothing about HTTP or terminals. It defines what a job is and how jobs are stored.
- **`internal/cli`** and **`internal/api`** translate their input (arguments, HTTP requests) into calls on `internal/job`, and translate results and errors back into output (text and exit codes, JSON and status codes).
- **`cmd/*`** only wire things up: read environment variables, create dependencies, start the program, and exit with the right code.

The CLI and the API do not share data yet. The CLI stores jobs in a JSON file; the API keeps them in memory. PostgreSQL replaces both in Phase 3.

## Domain: `internal/job`

### Job

| Field | Type | Notes |
|---|---|---|
| `id` | string | 32 hex characters from `crypto/rand` (unguessable) |
| `title` | string | Trimmed, 1–200 characters (counted as characters, not bytes) |
| `status` | `Status` | `pending`, `queued`, `processing`, `completed`, `failed` |
| `created_at`, `updated_at` | time | Stored in UTC |

`New(title)` is the only way to create a valid job. It returns `ErrEmptyTitle` or `ErrTitleTooLong` for bad input.

### MemoryStore

- A `map[string]*Job` guarded by a `sync.RWMutex`: `Lock` for `Add`/`Delete`, `RLock` for `Get`/`List`.
- Stores and returns **copies**, so no caller can modify stored jobs or race with another caller through a shared pointer.
- `List` returns jobs oldest first and never returns `nil` (so JSON shows `[]`, not `null`).
- Errors: `ErrNotFound`, `ErrDuplicateID`, `ErrNilJob`.

### File persistence (CLI only)

- `Load(path)` treats a missing file as an empty store and validates every job it reads.
- `Save(path)` writes to a temporary file, syncs it, then renames it over the real file, so a crash never leaves a half-written file.

## HTTP API: `internal/api` and `cmd/api`

### Request flow

Every request passes through middleware before reaching a handler:

```text
client
  │
  ▼
withRequestID   reuse a safe X-Request-ID or generate one; put it in the context and the response
  │
  ▼
logRequests     one structured log line per request: id, method, path, status, bytes, duration
  │
  ▼
recoverPanics   a handler panic becomes a logged 500 JSON response
  │
  ▼
jsonMuxErrors   the router's plain-text 404/405 become JSON
  │
  ▼
ServeMux        Go 1.22 patterns, e.g. "GET /api/v1/jobs/{id}"
  │
  ▼
handler         decode → validate → call the store → encode
```

`logRequests` sits outside `recoverPanics` so a recovered panic is logged with its final status (500).

### Errors

| Source | Status | Body |
|---|---|---|
| Bad Content-Type | 415 | `{"error": "Content-Type must be application/json"}` |
| Body over 1 MiB | 413 | `{"error": "body must not exceed 1048576 bytes"}` |
| Malformed JSON, unknown field, wrong type, empty or extra body | 400 | specific message |
| `ErrEmptyTitle` / `ErrTitleTooLong` | 400 | specific message |
| `ErrNotFound` | 404 | `{"error": "job not found"}` |
| Unknown route / wrong method | 404 / 405 | `{"error": "not found"}` / `{"error": "method not allowed"}` |
| Anything unexpected | 500 | `{"error": "internal error"}`; real cause logged with the request ID |

### Concurrency

`net/http` runs each request in its own goroutine. The only shared state is the `MemoryStore`, which is protected by its mutex. Every package's tests run with `-race`, and there are explicit tests with 100 concurrent goroutines.

### Lifecycle

1. `net.Listen` binds the port (fails fast if the port is taken), then logs `listening`.
2. `srv.Serve` runs in a goroutine.
3. On SIGINT/SIGTERM, the context is cancelled and `srv.Shutdown` stops accepting connections and waits up to 10 seconds for in-flight requests.
4. Server timeouts: read header 5s, read 10s, write 10s, idle 60s.

`run(ctx, logger, addr, ready)` holds all of this, so tests can start the real server on a random port and stop it by cancelling the context.

## Design decisions

| Decision | Reason |
|---|---|
| Standard library only | Learn what frameworks hide. Go 1.22's router covers method and path matching. |
| `internal/` for all packages | Nothing outside this module should import them. |
| Store returns copies | Prevents aliasing races once workers update jobs (Phase 6). |
| No store interface yet | Only one implementation exists. It will be introduced with PostgreSQL, when a second one does. |
| `{"jobs": [...]}` not a bare array | Room to add pagination fields without breaking clients. |
| Strict JSON decoding | Unknown fields fail loudly instead of being silently dropped. |
| Logs record path, never query | Query strings can carry tokens. |
| Request IDs validated | Prevents log injection and unbounded header values. |

## Known limitations

- API data is in memory and is lost on restart.
- CLI and API data are separate.
- Jobs are stored, but nothing processes them yet (queue and workers arrive in Phases 5 and 6).