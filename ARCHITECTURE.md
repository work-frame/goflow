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
- **`internal/worker`** and **`cmd/worker`** take IDs off the queue and run the jobs (see [The worker pool](#the-worker-pool)). **`internal/urlcheck`** holds the TLS rules both programs apply to their URLs.
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
3. If `GOFLOW_REDIS_URL` is set, connect to Redis (5 s ping) and start the relay and the reconciler, and, with `GOFLOW_WORKERS` above 0, the worker pool. Otherwise log a warning: new jobs wait in the outbox.
4. Start the hourly expired-session cleanup, and build the dummy bcrypt hash used for unknown emails in the background.
5. Bind the port (`GOFLOW_ADDR`, else `PORT`, else `:8080`), log `listening`, serve.
6. On `SIGINT`/`SIGTERM`: stop accepting connections, finish in-flight requests (up to 10 s), stop the cleanup and the relay, then close Redis and the database pool.

### The outbox

Creating a job writes two things to two systems: the job (PostgreSQL) and its ID on the queue (Redis). They can't be one transaction, so the API writes only to PostgreSQL:

```text
POST /api/v1/jobs ─► BEGIN; INSERT jobs; INSERT job_outbox; COMMIT ─► 201 ─► relay.Wake()
relay (at start, when woken, and on a retry timer after a failure):
  BEGIN
  SELECT ... FROM job_outbox ORDER BY id LIMIT 100 FOR UPDATE SKIP LOCKED
  LPUSH goflow:jobs:pending <ids>            ── fails? ROLLBACK; rows wait for the next try
  DELETE FROM job_outbox ...; UPDATE jobs SET status = 'queued' ...
  COMMIT
```

- **Nothing is lost.** A job exists only together with its outbox row. If Redis is down, the rows wait; the request still succeeds.
- **Delivered at least once, not exactly once.** If `LPUSH` succeeds and the `COMMIT` then fails, the rows remain and the same IDs are pushed again. Workers (Phase 6) must handle seeing a job twice.
- **Several instances are safe.** `FOR UPDATE SKIP LOCKED` gives each relay different rows.
- **`Wake` never blocks.** It's a send on a channel with room for one signal; extra wake-ups are dropped because the next pass sends everything anyway.
- **No polling while healthy.** After a failed pass the relay retries after 1 s, doubling up to 5 minutes; while all is well it doesn't query the database between wake-ups. Neon's free compute sleeps after 5 idle minutes and has 100 compute-hours a month; a query every 30 s would keep it awake around the clock (about 180 hours at the smallest size). `TestRelayDoesNotPollWhileHealthy` guards this.
- **The reconciler re-sends lost IDs.** The outbox gets an ID to Redis once; it can't see Redis lose it later (a free Key Value restart empties the lists). At start and then hourly, the API re-sends jobs `queued` for more than 10 minutes and moves their `updated_at` forward. A duplicate is harmless: `Claim` lets one worker win. One query an hour keeps Neon's cost small.

### The worker pool

`cmd/worker` runs `GOFLOW_WORKERS` goroutines (default 4). The API can run the same pool itself (`GOFLOW_WORKERS` above 0 in its environment), which is how it runs on Render. Each worker loops:

```text
Take      BLMOVE goflow:jobs:pending → goflow:jobs:processing   (atomic; waits up to 2 s)
Claim     UPDATE jobs SET status = 'processing' WHERE id = $1 AND status IN ('pending','queued')
            0 rows (already processing, finished or deleted) → skip, Ack
Handle    the work (a 5-second placeholder for now)
Complete  status = 'completed'   (or Fail → 'failed')
Ack       LREM goflow:jobs:processing
```

- **Exactly one claim wins, enforced by PostgreSQL.** When two workers claim the same job (a duplicate ID), the first `UPDATE` locks the row; the second waits for that commit, re-checks `status IN ('pending','queued')`, finds `processing` and changes 0 rows. This holds across processes, which a Go mutex can't. `TestClaimHasExactlyOneWinner` races two claims on real PostgreSQL 50 times.
- **An ID is never only in memory.** `BLMOVE` moves it between lists in one step, and only `Ack`, after the outcome is saved, removes it.
- **Shutdown has two signals.** `SIGTERM` cancels the pool's context, which stops workers from *taking* new jobs. Running jobs use a separate context (`context.WithoutCancel`) that `SIGTERM` doesn't reach; they get `DrainTimeout` (20 s) to finish, tracked with a `sync.WaitGroup`. Only after that are they cancelled: they record nothing, so the job stays `processing` and its ID stays in the `processing` list. An idle worker can delay shutdown by up to one `Take` (2 s): go-redis doesn't interrupt a blocking `BLMOVE` when its context is cancelled.
- **Recovery happens at the next start, not at shutdown.** A host may `SIGKILL` the process after its grace period, and no code runs then. So before any worker starts, `Pool.Run` resets jobs still `processing` to `queued` (`ResetProcessing`) and moves IDs left in the `processing` list back to the front of `pending` (`Recover`).
- **Single process only.** That startup recovery assumes every `processing` job belongs to a process that has stopped. **It is not safe with more than one worker process or API instance:** a second process starting would reset jobs the first is still running, and they would run twice at the same time. Running several processes needs leases (who holds a job, until when) so only expired claims are taken over (Phase 12). Goroutines inside one process are fine: they share one recovery, done before any of them start.
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
| Redis as the queue, next to PostgreSQL | Workers wait on `BLMOVE` instead of polling the database, which also lets a sleeping free database stay asleep. The cost: a second system to run and secure, and the dual-write problem, which the outbox exists to solve. PostgreSQL alone (`FOR UPDATE SKIP LOCKED`) would have avoided it; Redis was chosen to learn the queue patterns |
| Only job IDs travel through Redis | Nothing to serialize or keep in sync: the job lives in PostgreSQL, and a worker reads it by ID. Losing Redis's data loses no job, only "please run this" messages, which the outbox and `Recover` can resend |
| At least once, never at most once | A job may run twice (a crash after the work, before `Complete`), but it is never silently dropped. Duplicates of finished jobs are skipped by `Claim`; handlers with outside effects (email, payments) must be idempotent, for example by passing the job ID as the provider's idempotency key |
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
- One worker process at a time (see [The worker pool](#the-worker-pool)); not safe with several. It isn't deployed on Render yet.
- Jobs reach Redis at least once; a duplicate ID is possible after a failed commit. Workers must be idempotent (Phase 6–7).
- Plain `redis://` is accepted for single-label private-network names (Render's internal Key Value host): that traffic is unencrypted inside the provider's network.
- No rate limit on login or password change (Phase 9). Closed registration limits who has an account meanwhile.
- After the bcrypt cost changes, response time can tell old accounts from unknown emails until each user logs in once.
- A failed readiness check is logged twice, and every `/healthz` check from Render is logged (Phase 10).
- The CLI keeps one saved login at a time: logging in to another API replaces it.
- Hosting is free tier: the first request after idle waits for the server and database to wake ([DEPLOYMENT.md](DEPLOYMENT.md)).
