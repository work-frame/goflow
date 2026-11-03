# Architecture

This describes GoFlow at the end of Phase 7. It changes as each phase lands.

## Overview

```text
goflow (CLI)                                       api (server)
cmd/goflow ─► internal/cli ─► internal/client ──HTTP──► cmd/api ─► internal/api ─► internal/database ─► PostgreSQL
     │                                                                   │                    │
internal/credentials                                                     ├─── internal/job ───┤
(saved login, 0600)                                                      └─── internal/auth ──┘
                                                  (Job rules, pagination; users, passwords, session tokens)
```

The life of a job, from Phase 5 on:

```text
Step                          What happens                                          Status after
POST /api/v1/jobs             job row + outbox row, one transaction                 pending
relay                         outbox → LPUSH goflow:jobs:pending                    queued
worker                        BLMOVE → Claim (attempt + 1) → handler, 15 s limit    processing
  success                                                                           completed
  error, attempts left        PostgreSQL first, then ZADD goflow:jobs:delayed       retrying
  promoter, every 1 s         due IDs → goflow:jobs:pending; a worker claims again  processing
  permanent, or last attempt                                                        dead
POST /api/v1/jobs/{id}/retry  dead → attempts 0 + new outbox row, one transaction   pending
reconciler, start + hourly    re-sends from PostgreSQL whatever Redis lost          (unchanged)
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
| `Status` | `pending`, `queued`, `processing`, `retrying`, `completed`, `dead`; `failed` only on jobs from before retries |
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
- **`JobStore`** implements `Add`, `Get`, `GetByIdempotencyKey`, `List`, `Delete` and `Rerun`, always for one owner: every query has `user_id = $n`. Callers only ever see `job` errors: a unique violation becomes `ErrDuplicateKey` when it names the `jobs_user_idempotency_key` index and `ErrDuplicateID` otherwise, a missing row (or someone else's) becomes `ErrNotFound`, and a re-run of a job that isn't dead becomes `ErrNotRerunnable`.
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
  status     TEXT        NOT NULL     -- one of the 7 statuses
  created_at TIMESTAMPTZ NOT NULL
  updated_at TIMESTAMPTZ NOT NULL     -- never before created_at
  attempts        INT         NOT NULL DEFAULT 0   -- counted at every claim
  max_attempts    INT         NOT NULL DEFAULT 3   -- 1 to 10
  next_attempt_at TIMESTAMPTZ                      -- required while 'retrying'
  last_error      TEXT                             -- up to 1000 characters; not shown to users yet
)
INDEX (user_id, created_at, id)  -- one user's jobs, oldest first, paginated
INDEX (status, created_at)       -- status filter; workers in Phase 6
INDEX (next_attempt_at) WHERE status = 'retrying'  -- the reconciler's overdue retries

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
| `POST` | `/api/v1/jobs/{id}/retry` | `200` + job (`pending`, 0 attempts); `409` unless `dead` or `failed` |

Listing uses **keyset pagination**: `WHERE (created_at, id) > (cursor)`, `LIMIT limit + 1`. The extra row shows whether another page exists. Cursors are opaque base64url strings, validated on every request.

### Idempotent creation

A client that times out doesn't know whether its job was created. With an `Idempotency-Key` header it can simply send the request again:

1. The handler validates the key (1–255 visible ASCII characters, one header) and stores it on the job in the same `INSERT`.
2. A unique index on `(user_id, idempotency_key)` (partial: `WHERE idempotency_key IS NOT NULL`) lets only one job per key and owner exist. Two requests at the same moment both `INSERT`; the second waits for the first transaction and then fails with a unique violation naming that index. `JobStore.Add` turns that into `job.ErrDuplicateKey`.
3. The handler then reads the existing job by key and answers `200` with it and `Idempotent-Replayed: true`. Nothing new is stored or queued. A different title with the same key is `422`.

Checking "does this key exist?" in Go before inserting would let two concurrent requests both pass the check. The database is the only place that sees both requests (`TestIdempotentCreateUnderConcurrency`, 20 requests, one job). Names and status codes follow the IETF draft [`draft-ietf-httpapi-idempotency-key-header`](https://datatracker.ietf.org/doc/html/draft-ietf-httpapi-idempotency-key-header). Keys don't expire on their own; they go with their job.

**A replay returns the job as it is now, not a copy of the first response.** Live on Render, the first answer said `pending, 0 attempts` and the replay 18 ms later said `processing, 1 attempt`. Stripe-style APIs store the first response and send it again byte for byte; GoFlow reads the job instead. It's simpler (no response storage) and the client gets fresher data. The cost: a client must compare IDs, not whole bodies, to recognise a replay, or read the `Idempotent-Replayed` header.

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
3. If `GOFLOW_REDIS_URL` is set, create the Redis client and start the relay and the reconciler, and, with `GOFLOW_WORKERS` above 0, the worker pool. An unreachable Redis is logged (`queue not reachable yet`), not fatal: jobs still go to the outbox, the relay retries with backoff, and the pool retries its startup recovery until Redis answers (`TestAPIStartsWhenRedisIsDown`). Without `GOFLOW_REDIS_URL`, log a warning: new jobs wait in the outbox. `cmd/worker` still exits if Redis is down at start: it has nothing else to do.
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
Claim     UPDATE ... status = 'processing', attempts = attempts + 1
            WHERE id = $1 AND status IN ('pending','queued','retrying') RETURNING attempts, max_attempts
            0 rows (already processing, finished, dead or deleted) → skip, Ack
Handle    the work, under a 15 s deadline (worker.Demo: test behaviours picked by title)
Outcome   success                          → status = 'completed'
          Permanent(err), or last attempt  → status = 'dead', last_error
          any other error                  → status = 'retrying', next_attempt_at, last_error (COMMIT)
                                             then ZADD goflow:jobs:delayed <due ms> <id>
Ack       LREM goflow:jobs:processing
```

- **Exactly one claim wins, enforced by PostgreSQL.** When two workers claim the same job (a duplicate ID), the first `UPDATE` locks the row; the second waits for that commit, re-checks `status IN ('pending','queued','retrying')`, finds `processing` and changes 0 rows. This holds across processes, which a Go mutex can't. `TestClaimHasExactlyOneWinner` races two claims on real PostgreSQL 50 times.
- **An ID is never only in memory.** `BLMOVE` moves it between lists in one step, and only `Ack`, after the outcome is saved, removes it.
- **Shutdown has two signals.** `SIGTERM` cancels the pool's context, which stops workers from *taking* new jobs. Running jobs use a separate context (`context.WithoutCancel`) that `SIGTERM` doesn't reach; they get `DrainTimeout` (20 s) to finish, tracked with a `sync.WaitGroup`. Only after that are they cancelled: they record nothing, so the job stays `processing` and its ID stays in the `processing` list. An idle worker can delay shutdown by up to one `Take` (2 s): go-redis doesn't interrupt a blocking `BLMOVE` when its context is cancelled.
- **Recovery happens at the next start, not at shutdown.** A host may `SIGKILL` the process after its grace period, and no code runs then. So before any worker starts, `Pool.Run` resets jobs still `processing` to `queued` (`ResetProcessing`) and moves IDs left in the `processing` list back to the front of `pending` (`Recover`).
- **Single process only.** That startup recovery assumes every `processing` job belongs to a process that has stopped. **It is not safe with more than one worker process or API instance:** a second process starting would reset jobs the first is still running, and they would run twice at the same time. Running several processes needs leases (who holds a job, until when) so only expired claims are taken over (Phase 12). Goroutines inside one process are fine: they share one recovery, done before any of them start.
- **The worker doesn't migrate.** The API owns the schema.

### Retries

- **PostgreSQL is the truth, Redis only schedules.** A failed attempt is recorded first (`retrying`, `next_attempt_at`, `last_error`, in one statement); only after that commit is the ID added to the delayed set. If Redis then fails, or later loses the set (Key Value restart), the job is still `retrying` with a due time, and the reconciler re-sends it once it is 10 minutes overdue. The reconciler runs at startup and then hourly, so a retry lost with Redis's data comes back within about 70 minutes (10 minutes overdue plus up to an hour until the next run), sooner if the API restarts. Late, never lost. The other order could leave a retry in Redis that PostgreSQL never recorded (`TestRetryIsRecordedBeforeItIsScheduled`).
- **Backoff with equal jitter.** After attempt *n* fails, the wait is between half and all of 2 s × 2^(n−1), capped at 5 minutes: 1–2 s, 2–4 s, 4–8 s... The random half spreads out jobs that failed together; the fixed half stops a retry coming straight back.
- **Permanent errors skip retries.** A handler wraps an error with `worker.Permanent(err)` when trying again can't help (an invalid address). Unwrapped errors are retried: the safer default.
- **Attempts are counted at claim time,** so a job that crashes its worker also uses up attempts and can't loop forever. After `max_attempts` the job is `dead`.
- **The promoter** moves due IDs from the delayed set to `pending` every second, with a Lua script so the read, remove and push happen as one step and two promoters never deliver an ID twice. It only talks to Redis, so it doesn't wake the database.
- **Each attempt has a deadline** (`Pool.JobTimeout`, 15 s, below the 20 s drain). The handler's context ends then. What GoFlow records depends on what the handler does next:

  | After the deadline, the handler... | GoFlow records | Why |
  |---|---|---|
  | returns an error (usually `ctx.Err()`) | a failed attempt, `timed out after 15s`: `retrying` with backoff, or `dead` on the last attempt | An ordinary failure under the retry policy |
  | returns `nil` | `completed`, plus a warning `worker: job finished after its deadline` | The work is done; retrying would repeat it |
  | never returns (ignores `ctx`) | nothing; the worker stays busy until it does | Go can't kill a goroutine. With 2 workers, two such jobs stop all processing |

  Shutdown is kept apart from all three: when the pool's own context ends, nothing is recorded and the job runs again after the restart. Handlers must pass `ctx` to everything that can block ([Handler contract](#handler-contract)).
- **Dead jobs can be re-run by their owner.** `POST /api/v1/jobs/{id}/retry` (`goflow job retry`) runs one `UPDATE ... WHERE id AND user_id AND status IN ('dead','failed')` and inserts a new outbox row, in one transaction: the job is `pending` with 0 attempts and no `last_error`, and travels like a new job. The status in the `WHERE` clause makes a double click safe: the second request waits for the row lock, re-checks, finds `pending`, and gets `409`. There's no separate dead-letter list in Redis; `GET /api/v1/jobs?status=dead` is the dead-letter queue, kept in the system of record.
- **A retry may be claimed a moment early.** `Claim` accepts `retrying` without checking `next_attempt_at`: Redis decides when the ID arrives, and refusing it over a second of clock difference would drop it.

### Handler contract

A handler is the function that does a job's work (`worker.Handler`). The pool can't make a handler behave; it can only rely on these rules. Every real job type must follow them.

1. **Pass `ctx` to everything that can block, and return when it ends.** The 15-second deadline and shutdown both arrive only as a cancelled context. Go can't stop a goroutine from outside: a handler that ignores `ctx` keeps its worker busy until it returns, and with 2 workers on Render, two such handlers stop all processing (FAILURES.md, case 33).
2. **Expect to run more than once.** Delivery is at least once: a crash or a restart after the work but before `Complete`, a shutdown that cancels a running job, a timeout after the work was half done, and an owner's re-run all run the handler again. Work with outside effects must be idempotent: for an email or a payment, pass the job ID as the provider's idempotency key.
3. **Return `worker.Permanent(err)` when trying again can't help** (an invalid address, a missing file). Any other error is retried with backoff, up to `max_attempts`.
4. **Return `nil` only when the work is done.** GoFlow then records `completed`, even after the deadline: retrying finished work would do it twice, so a late success is logged as a warning, not treated as a failure. Never return `nil` for partial work.
5. **Keep errors free of secrets.** The error text is stored as `last_error` (up to 1,000 characters) and logged.

These are a contract, not something the pool enforces. Tests for each job type should check them: a cancelled context makes the handler return, a second run is harmless.

## CLI: `internal/cli`, `internal/client`, `cmd/goflow`

- `goflow job create | list [--status] | get | delete | retry` sends HTTP requests to `GOFLOW_API_URL` (default `http://localhost:8080`).
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
| Standard library first; dependencies are `pgx`, `go-redis`, `golang.org/x/crypto` (bcrypt) and `golang.org/x/term` | Learn what frameworks hide. Never write your own password hashing |
| Opaque session tokens stored as SHA-256 hashes, not JWTs | Logout and password changes end a session immediately. A copy of the `sessions` table can't be used to log in. Tokens are 256 random bits, so a fast hash is enough; passwords need bcrypt because people pick guessable ones |
| Ownership checked in SQL (`WHERE id = $1 AND user_id = $2`); another user's job is `404` | A forgotten check in a handler can't leak data, and the response doesn't confirm the job exists |
| The same `401` and the same time for every login failure | Response and timing can't reveal which emails have accounts |
| Registration closed by default | A public server doesn't accept strangers until there is a reason to |
| Remote database only over TLS (`verify-full`) | Passwords, hashes and session hashes never cross the internet in plain text |
| Redis as the queue, next to PostgreSQL | Workers wait on `BLMOVE` instead of polling the database, which also lets a sleeping free database stay asleep. The cost: a second system to run and secure, and the dual-write problem, which the outbox exists to solve. PostgreSQL alone (`FOR UPDATE SKIP LOCKED`) would have avoided it; Redis was chosen to learn the queue patterns |
| Only job IDs travel through Redis | Nothing to serialize or keep in sync: the job lives in PostgreSQL, and a worker reads it by ID. Losing Redis's data loses no job, only "please run this" messages, which the outbox and `Recover` can resend |
| At least once, never at most once | A job may run twice (a crash after the work, before `Complete`), but it is never silently dropped. Duplicates of finished jobs are skipped by `Claim`; handlers with outside effects (email, payments) must be idempotent, for example by passing the job ID as the provider's idempotency key |
| Transactional outbox instead of writing to PostgreSQL and Redis in the request | Two systems can't share a transaction. Writing to both loses jobs when the second write fails ([FAILURES.md](FAILURES.md), the dual-write experiment) |
| Retry state (attempts, status, next attempt time, last error) lives in PostgreSQL; Redis only schedules delivery | A Key Value restart empties every Redis structure. Whatever Redis loses, the reconciler can rebuild from PostgreSQL; the reverse would be impossible |
| **Equal jitter:** after attempt *n*, wait half of 2 s × 2^(n−1) plus a random amount up to the other half (1–2 s, 2–4 s, 4–8 s…, capped at 5 min) | Exact delays make jobs that failed together retry together, in spikes that hit a recovering service at the same moment. The random half spreads them out. The fixed half keeps every wait above zero, which full jitter (random from 0) doesn't |
| **Timeouts cancel; they don't decide the outcome.** Three cases, see [Retries](#retries) | Go can't stop a goroutine, so the deadline is only a signal. What the handler then does decides what GoFlow records |
| **A late success counts as `completed`** (logged as a warning) | The work happened. If an email went out after the deadline, marking the job failed would retry it and send the email twice. A deadline can cancel work; it can't undo it |
| **Lost Redis data is recovered from PostgreSQL by a reconciler that runs at startup and then hourly, for jobs more than 10 minutes overdue** | A retry or queued job whose ID Redis lost comes back 10–70 minutes late. Running it more often would keep Neon's free compute awake (100 CU-hours a month, Phase 6), and the 10-minute margin stops it re-sending IDs that are only waiting in a busy queue. Late is accepted; lost is not |
| The dead-letter queue is a status (`dead`), not a Redis list | One place to look (`?status=dead`), owner-scoped like every job, and a re-run is one transaction with the outbox |
| Idempotency enforced by a unique index; a replay returns the current job | Only the database sees concurrent requests. Reading the job avoids storing responses |
| CLI goes through the API | One set of rules; no back door around authentication |
| Interfaces defined by the code that uses them, only once a second implementation exists | `JobStore` and `Pinger` each have a real and a test implementation |
| Keyset pagination with opaque cursors | Fast at any depth; correct when jobs are added or removed between pages |
| Constraints in the database as well as in Go | The database outlives any one program |
| Migrations at startup under an advisory lock | Schema always matches the code; safe with several instances |
| Liveness never checks dependencies | A database outage shouldn't restart every API instance |
| Request context carried into every query | A disconnected client stops its query |
| Tests never use the development database | Tests can't damage dev data, or depend on it |

## Known limitations

- The work itself is a placeholder (`worker.Demo` picks a behaviour from the title); real job types with their own payloads come later.
- One worker process at a time (see [The worker pool](#the-worker-pool)); not safe with several. On Render the pool runs inside the single API instance. Leases for more instances come in Phase 12.
- Jobs reach Redis at least once, and a handler can run more than once. Handlers must follow the [handler contract](#handler-contract); nothing enforces it.
- When Key Value loses its data, retries and queued jobs come back through the hourly reconciler: up to about 70 minutes late.
- `last_error` is stored but not shown to users yet: `goflow job get` on a dead job doesn't say why it died.
- Idempotency keys never expire on their own; they're deleted with their job.
- Plain `redis://` is accepted for single-label private-network names (Render's internal Key Value host): that traffic is unencrypted inside the provider's network.
- No rate limit on login or password change (Phase 9). Closed registration limits who has an account meanwhile.
- After the bcrypt cost changes, response time can tell old accounts from unknown emails until each user logs in once.
- A failed readiness check is logged twice, and every `/healthz` check from Render is logged (Phase 10).
- The CLI keeps one saved login at a time: logging in to another API replaces it.
- Hosting is free tier: the first request after idle waits for the server and database to wake ([DEPLOYMENT.md](DEPLOYMENT.md)).
