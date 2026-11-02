# Architecture

This describes GoFlow at the end of Phase 4. It changes as each phase lands.

## Overview

```text
goflow (CLI)                                       api (server)
cmd/goflow ─► internal/cli ─► internal/client ──HTTP──► cmd/api ─► internal/api ─► internal/database ─► PostgreSQL
     │                                                                   │                    │
internal/credentials                                                     ├─── internal/job ───┤
(saved login, 0600)                                                      └─── internal/auth ──┘
                                                  (Job rules, pagination; users, passwords, session tokens)
```

- **One way in.** The CLI is an HTTP client of the API. Only the API knows the database URL, so every change to a job passes through the same validation, logging and authentication.
- **`internal/job`** is the domain: what a job is and the rules for listing jobs. It imports no HTTP or SQL packages.
- **`internal/auth`** is the account domain: users, email normalization, password hashing with bcrypt, and session tokens. Like `job`, it imports no HTTP or SQL packages.
- **`internal/queue`** owns Redis: a list of job IDs waiting for a worker (`goflow:jobs:pending`). Only IDs go through Redis; PostgreSQL stays the source of truth.
- **`internal/relay`** moves IDs from the outbox table to the queue in the background (see [The outbox](#the-outbox)).
- **`internal/worker`** and **`cmd/worker`** take IDs off the queue and run the jobs (see [The worker](#the-worker)). **`internal/urlcheck`** holds the TLS rules both programs apply to their URLs.
- **`internal/database`** owns PostgreSQL: connecting, migrating, and storing jobs, users and sessions.
- **`internal/api`** owns HTTP: routes, middleware, handlers.
- **`internal/client`** is a Go client for the API. **`internal/cli`** uses it. **`internal/credentials`** saves the login on disk.
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

## Accounts: `internal/auth`

| Item | Purpose |
|---|---|
| `User` | `id` (32 hex), `email` (trimmed, lowercased), `created_at`. `PasswordHash` is tagged `json:"-"`, so it can never be encoded into a response |
| `Hasher` | bcrypt at a configured cost (10–14 in production, 4 in tests). `NeedsRehash` reports a hash made at a different cost, so a login re-hashes it to the current one |
| `ValidatePassword` | 12 characters to 72 bytes (bcrypt ignores anything past 72 bytes) |
| `NewSession` | A token `gfs_` + 32 random bytes (base64url, 47 characters) and its SHA-256 hash. The token goes to the client once; only the hash is stored |
| `SessionTTL` | 24 hours |

## Storage: `internal/database`

- **`Open`** creates a `database/sql` pool using the `pgx` driver (10 open, 10 idle, 5-minute idle limit, 30-minute lifetime) and pings within 5 seconds.
- **`Migrate`** applies the SQL files embedded from `migrations/`, each in its own transaction together with its row in `schema_migrations`, while holding a PostgreSQL advisory lock. Several API instances can start at once safely.
- **`JobStore`** implements `Add`, `Get`, `List` and `Delete`, always for one owner: every query has `user_id = $n`. Callers only ever see `job` errors: a duplicate key becomes `ErrDuplicateID`, and a missing row (or someone else's) becomes `ErrNotFound`.
- **`UserStore`** creates users (a duplicate on `users_email_key` becomes `auth.ErrEmailTaken`), finds them by email, and changes a password hash. `ChangePassword` updates the hash and deletes the user's other sessions in one transaction.
- **`SessionStore`** creates, looks up (joined with the user, `expires_at > now()` on the database clock), deletes one, deletes all for a user, and deletes expired sessions.

### Schema

```sql
users (
  id            TEXT        PRIMARY KEY  -- 32 lowercase hex characters
  email         TEXT        NOT NULL UNIQUE  -- stored lowercased (CHECK)
  password_hash TEXT        NOT NULL     -- must look like a bcrypt hash (CHECK)
  created_at    TIMESTAMPTZ NOT NULL
)

sessions (
  token_hash BYTEA       PRIMARY KEY     -- SHA-256 of the token, 32 bytes
  user_id    TEXT        NOT NULL REFERENCES users ON DELETE CASCADE
  created_at TIMESTAMPTZ NOT NULL
  expires_at TIMESTAMPTZ NOT NULL        -- after created_at
)
INDEX (user_id)     -- log out everywhere
INDEX (expires_at)  -- hourly cleanup

jobs (
  id         TEXT        PRIMARY KEY  -- 32 lowercase hex characters
  user_id    TEXT        NOT NULL REFERENCES users ON DELETE CASCADE
  title      TEXT        NOT NULL     -- 1 to 200 characters
  status     TEXT        NOT NULL     -- one of the 5 statuses
  created_at TIMESTAMPTZ NOT NULL
  updated_at TIMESTAMPTZ NOT NULL     -- never before created_at
)
INDEX (user_id, created_at, id)  -- one user's jobs, oldest first, paginated
INDEX (status, created_at)       -- status filter; workers in Phase 6

job_outbox (
  id         BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY  -- delivery order
  job_id     TEXT        NOT NULL UNIQUE REFERENCES jobs ON DELETE CASCADE
  created_at TIMESTAMPTZ NOT NULL
)
```

Migration 6 gave every job that was still `pending` an outbox row, so jobs created before the queue existed get queued too.

Migration 4 deleted every job that existed before accounts did, because no user could own them. It runs in one transaction, so a failure rolls the deletion back too.

Every rule is also a `CHECK` constraint, so bad data is rejected even if it doesn't come from GoFlow.

## HTTP API: `internal/api` and `cmd/api`

### Request flow

```text
client → withRequestID → logRequests → recoverPanics → jsonMuxErrors → ServeMux → requireSession → handler → stores
```

`requireSession` wraps every endpoint except `healthz`, `readyz`, `register` and `login`. It reads `Authorization: Bearer <token>`, checks the token's format, hashes it and looks the hash up. Every failure (no header, wrong scheme, bad format, unknown, expired) gets the same `401` with `WWW-Authenticate: Bearer realm="goflow"`, so the response says nothing about why. The user ID then travels in the request context. Handlers read it with `callerID`, which panics if there is none, so a route that forgot `requireSession` fails loudly instead of serving everyone's data.

Every storage call gets `context.WithTimeout(r.Context(), 3s)`: it stops when the client disconnects or after 3 seconds, whichever comes first.

### Endpoints

| Method | Path | Success |
|---|---|---|
| `GET` | `/healthz` | `200`: the process is alive (checks nothing else) |
| `GET` | `/readyz` | `200`: the database answers a ping within 2 s, else `503` |
| `POST` | `/api/v1/auth/register` | `201` + user, or `403` when registration is closed |
| `POST` | `/api/v1/auth/login` | `200` + `{"token", "expires_at"}`, `Cache-Control: no-store` |
| `POST` | `/api/v1/auth/logout` | `204`: this session ends |
| `POST` | `/api/v1/auth/logout-all` | `200` + `{"sessions_ended": n}` |
| `POST` | `/api/v1/auth/password` | `200` + `{"sessions_ended": n}`: other sessions end |
| `GET` | `/api/v1/auth/me` | `200` + user |
| `POST` | `/api/v1/jobs` | `201` + job + `Location` |
| `GET` | `/api/v1/jobs?status=&limit=&cursor=` | `200` + `{"jobs": [...], "next_cursor": "..."}` |
| `GET` | `/api/v1/jobs/{id}` | `200` + job |
| `DELETE` | `/api/v1/jobs/{id}` | `204` |

Listing uses **keyset pagination**: `WHERE (created_at, id) > (cursor)`, `LIMIT limit + 1`. The extra row shows whether another page exists. Cursors are opaque base64url strings, validated on every request.

### Errors

All errors are `{"error": "..."}`.

| Status | When |
|---|---|
| 400 | Invalid body, title, status, limit, cursor, email or password |
| 401 | Missing, unknown or expired session; wrong email or password at login |
| 403 | Registration closed; wrong current password on a password change |
| 404 / 405 | Unknown job (or another user's) or route / wrong method |
| 409 | Email already registered |
| 413 / 415 | Body over 1 MiB / not `application/json` |
| 499 | Client disconnected (logged at Info; not a server error) |
| 503 | Storage timed out |
| 500 | Anything else. The cause is logged with the request ID, never sent to the client |

### Lifecycle

1. Read every setting with `loadConfig` and report all problems at once (`errors.Join`): a missing or non-TLS remote `GOFLOW_DATABASE_URL`, a bcrypt cost outside 10–14, a `GOFLOW_REGISTRATION` other than `open` or `closed`. Errors never include the URL, which holds the password.
2. Connect (5 s ping), run migrations. No port is opened if this fails.
3. If `GOFLOW_REDIS_URL` is set, connect to Redis (5 s ping) and start the relay. Otherwise log a warning: new jobs wait in the outbox.
4. Start the hourly expired-session cleanup, and build the dummy bcrypt hash used for unknown emails in the background.
5. Bind the port (`GOFLOW_ADDR`, else `PORT`, else `:8080`), log `listening`, serve.
6. On `SIGINT`/`SIGTERM`: stop accepting connections, finish in-flight requests (up to 10 s), stop the cleanup and the relay, then close Redis and the database pool.

### The outbox

Creating a job writes two things to two systems: the job (PostgreSQL) and its ID on the queue (Redis). They can't be one transaction, so the API writes only to PostgreSQL:

```text
POST /api/v1/jobs ─► BEGIN; INSERT jobs; INSERT job_outbox; COMMIT ─► 201 ─► relay.Wake()
relay (every 30 s, and when woken):
  BEGIN
  SELECT ... FROM job_outbox ORDER BY id LIMIT 100 FOR UPDATE SKIP LOCKED
  LPUSH goflow:jobs:pending <ids>            ── fails? ROLLBACK; rows wait for the next try
  DELETE FROM job_outbox ...; UPDATE jobs SET status = 'queued' ...
  COMMIT
```

- **Nothing is lost.** A job exists only together with its outbox row. If Redis is down, the rows wait; the request still succeeds.
- **Delivered at least once, not exactly once.** If `LPUSH` succeeds and the `COMMIT` then fails, the rows remain and the same IDs are pushed again. Workers (Phase 6) must handle seeing a job twice.
- **Several instances are safe.** `FOR UPDATE SKIP LOCKED` gives each relay different rows.
- **`Wake` never blocks.** It's a send on a channel with room for one signal; extra wake-ups are dropped because the next pass sends everything anyway. The 30 s tick covers wake-ups that never came.

### The worker

```text
Take      BLMOVE goflow:jobs:pending → goflow:jobs:processing   (atomic; waits up to 5 s)
Claim     UPDATE jobs SET status = 'processing' WHERE status IN ('pending','queued','processing')
            no row (completed, failed, deleted) → skip, Ack
Handle    the work (a 5-second placeholder for now)
Complete  status = 'completed'   (or Fail → 'failed')
Ack       LREM goflow:jobs:processing
```

- **An ID is never only in memory.** `BLMOVE` moves it between lists in one step, so a worker that dies mid-job leaves it in `processing`. On start, `Recover` moves those IDs back to the front of `pending`.
- **Duplicates are harmless.** Delivery is at least once. The second copy of an ID finds the job `completed` and is skipped: the job runs once, the ID is acknowledged twice.
- **Saved before acknowledged.** The outcome is written to PostgreSQL before the `Ack`. A crash in between leaves the ID in `processing`; after recovery `Claim` sees the job finished and skips it.
- **Stopping mid-job records nothing.** On `Ctrl+C` the job stays `processing` and its ID stays in `processing`; it runs again after the next start.
- **One worker only, for now.** `Recover` can't tell a dead worker's IDs from a live one's, and `Claim` lets a `processing` job be claimed again. Both are safe with one worker; several workers need leases and heartbeats (Phases 6 and 12).
- **The worker doesn't migrate.** The API owns the schema.

## CLI: `internal/cli`, `internal/client`, `cmd/goflow`

- `goflow job create | list [--status] | get | delete` sends HTTP requests to `GOFLOW_API_URL` (default `http://localhost:8080`).
- `goflow login` reads the password from the terminal without echo (`golang.org/x/term`) and saves `{api_url, token, expires_at}` in `~/.config/goflow/credentials`: written to a temporary file with mode `0600`, then renamed into place, inside a `0700` directory. The CLI refuses a file that group or others can read, and sends the token only to the API that issued it and only until it expires. `GOFLOW_TOKEN` overrides the file for scripts. `goflow logout` ends the session on the server, then deletes the file.
- Each command has a 90-second deadline (long enough for a sleeping free server to wake), and `Ctrl+C` cancels the request in flight.
- `list` follows `next_cursor` until the last page.
- The client turns a 404 `job not found` back into `job.ErrNotFound`, reports other non-success responses as `*client.APIError`, and reports network failures as ordinary wrapped errors.

## Testing

| Layer | Approach |
|---|---|
| `internal/job`, `internal/auth` | Plain unit tests, including the pagination rules and token formats |
| `internal/credentials` | Real files in `t.TempDir()`: modes, refusals, which API gets the token |
| `internal/api` | Handlers with fakes: an adapter over `MemoryStore`, an always-failing store, and a spy that records the context it was given |
| `internal/cli` | A fake API with 2 jobs per page, plus failure and deadline doubles |
| `internal/client` | **Contract tests** against the real `api.NewRouter`, served by `httptest.NewServer` |
| `internal/database` | Real PostgreSQL. `TestMain` migrates one template database per run, and each test copies it |
| `cmd/api` | End to end: the real server on `127.0.0.1:0`, including jobs surviving a restart, sign-up, sessions, and settings validation |

Everything runs with the race detector (`./scripts/check.sh`). [FAILURES.md](FAILURES.md) records what each safeguard prevents.

## Design decisions

| Decision | Reason |
|---|---|
| Standard library first; dependencies are `pgx`, `golang.org/x/crypto` (bcrypt) and `golang.org/x/term` | Learn what frameworks hide. Never write your own password hashing |
| Opaque session tokens stored as SHA-256 hashes, not JWTs | Logout and password changes end a session immediately. A copy of the `sessions` table can't be used to log in. Tokens are 256 random bits, so a fast hash is enough; passwords need bcrypt because people pick guessable ones |
| Ownership checked in SQL (`WHERE id = $1 AND user_id = $2`); another user's job is `404` | A forgotten check in a handler can't leak data, and the response doesn't confirm the job exists |
| The same `401` and the same time for every login failure | Response and timing can't reveal which emails have accounts |
| Registration closed by default | A public server doesn't accept strangers until there is a reason to |
| Remote database only over TLS (`verify-full`) | Passwords, hashes and session hashes never cross the internet in plain text |
| Transactional outbox instead of writing to PostgreSQL and Redis in the request | Two systems can't share a transaction. Writing to both loses jobs when the second write fails ([FAILURES.md](FAILURES.md), the dual-write experiment) |
| CLI goes through the API | One set of rules; no back door around authentication |
| Interfaces defined by the code that uses them, only once a second implementation exists | `JobStore` and `Pinger` each have a real and a test implementation |
| Keyset pagination with opaque cursors | Fast at any depth; correct when jobs are added or removed between pages |
| Constraints in the database as well as in Go | The database outlives any one program |
| Migrations at startup under an advisory lock | Schema always matches the code; safe with several instances |
| Liveness never checks dependencies | A database outage shouldn't restart every API instance |
| Request context carried into every query | A disconnected client stops its query |
| Tests never use the development database | Tests can't damage dev data, or depend on it |

## Known limitations

- The work itself is a placeholder; real job types, retries and timeouts come in Phases 6–7.
- One worker process at a time (see [The worker](#the-worker)). It isn't deployed on Render yet.
- Jobs reach Redis at least once; a duplicate ID is possible after a failed commit. Workers must be idempotent (Phase 6–7).
- Every API instance with Redis polls the outbox every 30 s. On a database that sleeps when idle (Neon free), that keeps it awake: decide in Phase 6, when Redis is hosted.
- No rate limit on login or password change (Phase 9). Closed registration limits who has an account meanwhile.
- After the bcrypt cost changes, response time can tell old accounts from unknown emails until each user logs in once.
- A failed readiness check is logged twice, and every `/healthz` check from Render is logged (Phase 10).
- The CLI keeps one saved login at a time: logging in to another API replaces it.
- Hosting is free tier: the first request after idle waits for the server and database to wake ([DEPLOYMENT.md](DEPLOYMENT.md)).
