# Architecture

This describes GoFlow at the end of Phase 12. It changes as each phase lands.

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
- **`internal/events`** carries job changes to the browsers watching them (see [Live updates](#live-updates)).
- **`internal/relay`** moves IDs from the outbox table to the queue in the background (see [The outbox](#the-outbox)).
- **`internal/worker`** and **`cmd/worker`** take IDs off the queue and run the jobs (see [The worker pool](#the-worker-pool)). **`internal/urlcheck`** holds the TLS rules both programs apply to their URLs.
- **`internal/database`** owns PostgreSQL: connecting, migrating, and storing jobs, users and sessions.
- **`internal/api`** owns HTTP: routes, middleware, handlers.
- **`internal/client`** is a Go client for the API. **`internal/cli`** uses it. **`internal/credentials`** saves the login on disk.
- **`cmd/*`** only wire things together: read environment variables, build dependencies, run, exit.

## Domain: `internal/job`

| Item | Purpose |
|---|---|
| `Job` | `id` (32 hex, `crypto/rand`), `title` (1–200 characters), `status`, `created_at`, `updated_at` (UTC, microsecond precision), `attempts`, `max_attempts`, `next_attempt_at`, `version` (1, then +1 per change) |
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
  idempotency_key TEXT                             -- 1 to 255 characters; NULL without the header
  version         BIGINT      NOT NULL DEFAULT 1   -- every UPDATE adds 1
  lease_token     TEXT                             -- the worker holding a processing job (Task 48)
  lease_expires_at TIMESTAMPTZ                     -- when that lease runs out, PostgreSQL's clock
  CHECK (status = 'processing') = (lease_token IS NOT NULL AND lease_expires_at IS NOT NULL)
)
UNIQUE INDEX (user_id, idempotency_key) WHERE idempotency_key IS NOT NULL
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
| `GET` | `/metrics` | `200`, Prometheus text, with the metrics token; `401` without; absent (`404`) when no token is set |
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

### Live updates

`GET /api/v1/events` is a Server-Sent Events stream of the caller's job changes. The path a change takes:

```text
store commits a change ─► Notifier (events.Publisher): buffer of 256, never blocks
                          └─► one goroutine: PUBLISH goflow:events {"user_id", "message"}
every API instance: SUBSCRIBE goflow:events ─► events.Hub ─► only the owner's open streams (buffer 64 each)
                                                                └─► data: {"type":"changed","job":{...}}
```

- **PostgreSQL stays the truth; events are hints.** Stores call the `Notifier` only after `COMMIT`, with the row as committed (`RETURNING`), so a listener never hears about a change that was rolled back. Redis Pub/Sub delivers at most once and keeps nothing. A missed event costs a client a reload, never a job. Every status change goes through a store method (`Add`, `Rerun`, `Delete`, `Relay`, `Claim`, `Complete`, `Retry`, `Bury`, `Reclaim`, `ReclaimExpired`), so none can be forgotten in a handler.
- **Publishing never slows the real work.** `events.Publisher` encodes the event, puts it in a buffer and returns. One goroutine sends to Redis, in order, with a 2-second limit per message. When Redis hangs, the buffer fills and new events are dropped and counted. A Redis outage can't delay a job's creation or a worker's result.
- **Routing by owner, in the hub.** Every API instance subscribes once to one channel. The `Hub` hands each message only to the streams of the job's owner (`user_id` from the Redis envelope, which never reaches the client). This is the same rule as `WHERE user_id = $n` in SQL, applied to events (FAILURES.md, case 35).
- **A slow client is dropped, not waited for.** Delivery never blocks. A stream more than 64 messages behind is closed; its client reconnects and reloads.
- **No gap between the snapshot and the stream.** The handler subscribes to the hub, waits until the API's Redis subscription is live, and only then sends `ready`. The client loads its jobs after `ready`, so any change committed after that read arrives as an event. When the Redis subscription drops and comes back, every stream gets `resync`, because messages published in between are gone. A longer break is reported while it lasts: see [Delivery health](#delivery-health).
- **Order isn't guaranteed; `version` decides.** The relay and a worker announce from different goroutines (or processes), so `processing` can arrive before `queued`. A client keeps an event only if its `version` is higher than its copy's. `queued` is often skipped altogether: the relay pushes the ID before it marks the job queued, and a fast worker claims it first.
- **Why a version, not `updated_at`.** Every `UPDATE` of a job sets `version = version + 1` in the same statement (migration 0009). PostgreSQL's row lock makes updates of one row take turns, so versions follow commit order exactly. `updated_at` can't: `now()` is the time the transaction *started*, so a transaction that starts first, waits for the row lock and commits second writes the older time on the newer state, and two changes can get the same time. `TestEveryJobUpdateBumpsTheVersion` reads the package's source and fails if any `UPDATE jobs` forgets the version (FAILURES.md, case 36).
- **Tickets, not tokens, in the URL.** `EventSource` can't send an `Authorization` header. `POST /api/v1/events/ticket` (with the session) returns `gft_` + 32 random bytes; Redis stores its SHA-256 hash for 30 seconds (`SET NX EX`), and the stream redeems it with `GETDEL`, so it works exactly once, even with two requests racing. A ticket in a proxy log is already spent. Our own request log never records query strings.
- **Long-lived responses.** The server's 10-second read and write timeouts are lifted for each stream with `http.ResponseController`, which reaches the connection through the middleware's `Unwrap` methods. A heartbeat comment every 25 seconds keeps proxies from closing a quiet stream; it's outbound, so it doesn't keep a free Render instance awake. `Shutdown` waits for open connections, so `RegisterOnShutdown(hub.Close)` ends every stream the moment shutdown starts.
- **Without Redis,** the ticket and stream endpoints answer `503` and nothing else changes.

### Delivery health

An open stream proves only that the browser reaches the API. Whether job events reach the API is a separate question, and before Phase 10 nothing asked it: if the API lost its Redis subscription, events stopped, the stream stayed open and the dot said `live` over a list that wasn't moving. Resync *repaired* missed events once the subscription came back; nothing *detected* that they had stopped. The requirement was written down at the end of Phase 8 and built in Task 42 (`internal/events/health.go`):

```text
every 2 s   events.Watch ─► PUBLISH goflow:events {"canary":"<this hub's ID>"}
            events.Listen ◄── the same channel ◄── Redis
              own canary back ─► hub.heard(): last heard = now
            no canary back for 8 s ─► hub.check(): paused ─► {"type":"paused"} to every stream
            next canary back ─► {"type":"resumed"} to every stream ─► clients reload from PostgreSQL
```

1. **Health means "a message published now reaches this hub".** The API publishes a canary to the same channel job events use, every 2 seconds, whether or not any job is moving: a quiet system and a broken one look the same from outside. Only a canary that came **back** counts. A successful `PUBLISH` proves Redis took the message, not that this API's subscription delivered it (FAILURES.md, case 42). Each hub has a random ID and counts only its own canaries, since every API instance publishes on the same channel.
2. **Loss within 10 seconds, recovery at the next canary.** No canary back for 8 seconds, checked every 2, is a pause: reported 8–10 seconds after the break. The first canary that comes back ends it. go-redis re-subscribing isn't enough: during a pause, a resubscription sends nothing, and `resumed` follows with the first canary through it. A drop that go-redis repairs in a fraction of a second (a killed connection) never becomes a pause; streams get the usual `resync` (`TestShortRedisDropIsOnlyAResync`).
3. **Every stream is told, and none is closed.** The hub broadcasts `{"type":"paused"}` and `{"type":"resumed"}` to every open stream; like `resync`, they carry no job data. The state changes and the message is queued under the hub's lock, and a new stream checks `Paused()` after it has subscribed: one that opens during a pause gets `paused` right after `ready`, and can't miss a change that comes after its check (at worst it hears `paused` twice).
4. **The dot reports the whole path.** Three states: `live` (stream open, events arriving), `paused` (stream open, events not reaching the server: the list may be stale) and `offline`/`reconnecting` (stream closed). After `resumed` the page reloads its list, as after `resync`.
5. **Tested with the stream open.** `TestLiveDeliveryPausesWhileRedisIsAway` runs the real server with its Redis behind a small TCP proxy, opens a stream, cuts the proxy, and expects `paused` (about 1 s after the cut with the test's 100 ms / 1 s timings) **without the stream ending**, `goflow_event_delivery_ok 0`, then `resumed` once the proxy is back. The requirement suggested `CLIENT KILL TYPE pubsub`; it isn't used because it disconnects every subscriber on the shared test Redis, other test packages' included, and because go-redis reconnects within about 100 ms, so it produces a `resync`, never a pause. The proxy breaks only this server's connections. The page's side is checked by `TestScriptHandlesEveryMessageType` (every message type has a case; `paused` sets the paused state; `ready`, `resync` and `resumed` reload), and was checked once in a headless Chromium: Redis cut, the dot said `paused: updates delayed` 8.3 s later on the same stream; a job created meanwhile wasn't shown; Redis back, `live` 0.2 s later, and the job appeared.

The cost: one `PUBLISH` every 2 seconds per API instance, about 43,000 a day. Render's Key Value docs mention no command quota (only memory and connections). On a plan that does (Upstash's free plan: 500,000 a month), the canary alone would need 1.3 million, so the interval would have to grow.

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
3. If `GOFLOW_REDIS_URL` is set, create the Redis client and start the relay, the reconciler and the lease reaper, and, with `GOFLOW_WORKERS` above 0, the worker pool. An unreachable Redis is logged (`queue not reachable yet`), not fatal: jobs still go to the outbox, the relay retries with backoff, and the pool retries its startup recovery until Redis answers (`TestAPIStartsWhenRedisIsDown`). Without `GOFLOW_REDIS_URL`, log a warning: new jobs wait in the outbox. `cmd/worker` still exits if Redis is down at start: it has nothing else to do.
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
Claim     UPDATE ... status = 'processing', attempts = attempts + 1,
                     lease_token = <new random token>, lease_expires_at = now() + 30 s
            WHERE id = $1 AND status IN ('pending','queued','retrying') RETURNING ..., lease_expires_at
            0 rows (already processing, finished, dead or deleted) → skip, Ack
          ZADD goflow:jobs:leases <expiry ms> <id>
Handle    the work, under a 15 s deadline (worker.Demo: test behaviours picked by title)
          every 10 s: UPDATE ... lease_expires_at = now() + 30 s WHERE id AND lease_token = <token>
            0 rows → the lease is gone: cancel the handler, record nothing
Outcome   (every UPDATE also WHERE lease_token = <token>, and ends the lease)
          success                          → status = 'completed'
          Permanent(err), or last attempt  → status = 'dead', last_error
          any other error                  → status = 'retrying', next_attempt_at, last_error (COMMIT)
                                             then ZADD goflow:jobs:delayed <due ms> <id>
          0 rows                           → the lease was lost: result discarded
          ZREM goflow:jobs:leases
Ack       LREM goflow:jobs:processing
```

- **Exactly one claim wins, enforced by PostgreSQL.** When two workers claim the same job (a duplicate ID), the first `UPDATE` locks the row; the second waits for that commit, re-checks `status IN ('pending','queued','retrying')`, finds `processing` and changes 0 rows. This holds across processes, which a Go mutex can't. `TestClaimHasExactlyOneWinner` races two claims on real PostgreSQL 50 times.
- **An ID is never only in memory.** `BLMOVE` moves it between lists in one step, and only `Ack`, after the outcome is saved (or found to be someone else's to save), removes it.
- **Shutdown has two signals.** `SIGTERM` cancels the pool's context, which stops workers from *taking* new jobs. Running jobs use a separate context (`context.WithoutCancel`) that `SIGTERM` doesn't reach; they get `DrainTimeout` (20 s) to finish, tracked with a `sync.WaitGroup`. Only after that are they cancelled: they record nothing, so the job stays `processing` until its lease runs out and the reaper sends it elsewhere. An idle worker can delay shutdown by up to one `Take` (2 s): go-redis doesn't interrupt a blocking `BLMOVE` when its context is cancelled. That 2 s (`worker.DefaultTakeTimeout`) is kept on purpose in production. Tests set `Pool.TakeTimeout` to 1 s, the shortest go-redis allows (it rounds a blocking timeout under 1 s up to 1 s), because their servers stop at the end of every test (Task 52).
- **Leases make several processes safe (Task 48).** Before, every start reset all `processing` jobs to `queued`, which was only correct with one process: a second one starting reset jobs the first was still running, and they ran again (`TestSeveralProcessesRunEachJobOnce` saw attempt 3 on the old code). Now a `processing` job belongs to whoever holds its lease:
  - **Every claim gets a fresh token** (`crypto/rand.Text`, 130 bits), and `lease_expires_at = now() + 30 s`, computed by PostgreSQL. A database constraint ties the two together: a `processing` job always has a lease, nothing else ever does.
  - **The token fences every write.** `Renew`, `Complete`, `Retry` and `Bury` all say `WHERE lease_token = $token`; once the job has been taken back (and maybe claimed by another worker), the old holder's token matches nothing, and it gets `ErrLeaseLost`. A worker that loses its lease therefore can't overwrite what its replacement does, even in the worst case: its handler finishes and it calls `Complete` while the other worker is mid-job, the job still `processing` (FAILURES.md, case 46; `TestAFrozenWorkerCannotOverwriteItsReplacement`).
  - **The heartbeat** renews the lease every 10 seconds. When the database says the lease is gone, or renewals have failed for a whole lease length (it can't know whether the job was taken back), the worker cancels its handler with `errLeaseLost` as the cause and records nothing.
  - **PostgreSQL's clock decides expiry; Redis only says when to ask.** Every deadline is also kept in the sorted set `goflow:jobs:leases`. The lease reaper (`relay.LeaseReaper`, in the API and in every worker process) checks it every 5 seconds and asks PostgreSQL about each deadline that has passed. `Reclaim` takes the job back only `WHERE lease_expires_at < now()` there; if the lease was renewed, or the reaper's clock is fast, it answers with the real deadline instead. With no deadline passed, PostgreSQL isn't touched, so a sleeping free database stays asleep. At start and hourly the reaper also sweeps PostgreSQL directly (`ReclaimExpired`), for deadlines a Key Value restart lost.
  - **One winner in every race.** The row lock serialises a late `Complete` against `Reclaim`, and a `Renew` against `Reclaim`: exactly one takes effect, never both, never neither (`TestCompleteAndReclaimRaceHasOneWinner`, `TestRenewAndReclaimRaceHasOneWinner`, 30 rounds each on real PostgreSQL). Until someone takes it back, an expired lease still counts: no other worker can have the job, so its holder may renew it or record its result.
  - **Seen with a real crash (Task 49).** In the Compose stack (the API and three worker containers), `docker kill` on one worker 3 s into two 12-second jobs left them `processing` on attempt 1 with a deadline nobody renewed, while the heartbeat moved the other six jobs' deadlines. With 1 s left they were still untouched; 0.57 s after the deadline the API's reaper took them back, and `worker-1` and the API ran them as attempt 2. The other six jobs finished on attempt 1 (FAILURES.md, experiment "a worker killed with SIGKILL"). `TestAFrozenWorkerCannotOverwriteItsReplacement` checks the same order on PostgreSQL's clock: the replacement's claim is later than the frozen worker's deadline. DEVELOPMENT.md has the steps to repeat it with `docker kill`.
  - **A lost lease uses up the attempt,** like a crash. If it was the last one, the job becomes `dead` with `last_error` saying so, instead of going round forever.
  - **Startup never touches a running job.** A starting pool only tidies the shared `processing` list: an ID whose job is `processing` belongs to someone's lease, so its stale entry is dropped; any other ID was taken but never claimed (or finished but not acknowledged), so it goes back to the queue (`Requeue`, a Lua script, so two processes starting together can't both push it). If a live worker in another process had just taken that ID, the copy is only a duplicate, and `Claim` lets one worker win.
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

## Metrics: `internal/metrics`

`GET /metrics` serves Prometheus's text format: GoFlow's own metrics plus Go's (goroutines, memory, GC) and the process's (CPU, open files).

- **Updated where things happen, never computed when read.** A middleware counts each finished request and records its duration. Reading `/metrics` only formats numbers already in memory: it never touches PostgreSQL, so polling it can't keep Neon awake. (It does keep a free Render instance awake, like any request; that's why nothing polls it yet.)
- **Labelled by route pattern, never by path.** The label is the pattern the mux matched (`GET /api/v1/jobs/{id}`, from `r.Pattern`), so every job ID shares one time series. Requests that matched no route are all `unmatched`. A path label would give anyone who sends made-up URLs a new series per request, until the monitoring service's limit or the server's memory runs out (FAILURES.md, case 40). The middleware sits outside `recoverPanics`, so a panic counts as the `500` it became, and right where the mux's `r` is the same request: a `r.WithContext` in between would hide the pattern.
- **Event streams** are counted but kept out of the duration histogram: a stream lasts as long as its browser tab.
- **GoFlow's own metrics** (Task 41), each label from a fixed set:

  | Metric | Type | Updated |
  |---|---|---|
  | `goflow_job_status_changes_total{status}` | counter | by the stores after each commit: `Metrics` is a `database.Notifier`, given to the stores together with the events publisher (`database.NotifyAll`), so no status change can be missed. `pending` includes re-runs; `deleted` counts deletions |
  | `goflow_job_attempt_duration_seconds{outcome}` | histogram | by the worker pool (`Pool.Observe`) once an attempt's outcome is saved: `completed`, `retry` or `dead`. Workers in `cmd/worker` aren't measured: it has no `/metrics` |
  | `goflow_queue_jobs{list}` | gauge | when `/metrics` is read: one Redis round trip (`LLEN`, `LLEN`, `ZCARD`) with a 1 s limit, never PostgreSQL. **Missing, not zero, when Redis doesn't answer** (FAILURES.md, case 41) |
  | `goflow_events_published_total`, `goflow_events_dropped_total` | counter | by the events publisher |
  | `goflow_event_streams` | gauge | open event streams on this instance |
  | `goflow_rate_limited_total{rule}` | counter | each `429` |
  | `goflow_rate_limit_fallback` | gauge | 1 while limits are counted in memory because Redis is unreachable |
  | `goflow_event_delivery_ok` | gauge | 1 while this API's canaries come back, 0 while live delivery is paused (Task 42) |

  Unknown and zero are different answers. A graph that drops to zero during an outage says "nothing waiting"; a gap says "we don't know", which is the truth.
- **Behind a token.** `GOFLOW_METRICS_TOKEN` (at least 32 characters; Render generates one) is compared in constant time via SHA-256, so neither its content nor its length leaks through timing. Without a token the route doesn't exist. The numbers show how busy GoFlow is and how it fails: nobody else's business.
- **Own registry.** Each `metrics.New()` has its own registry, not Prometheus's global one, so tests can start any number of servers.
- **Collected locally (Task 46).** `docker compose --profile monitoring` adds Prometheus, which reads `/metrics` every 15 seconds with the development token, and Grafana with a provisioned **GoFlow overview** dashboard: requests and 5xx per route, 95th-percentile request and attempt times (`histogram_quantile` over the buckets), jobs finished per hour (`increase(...[1h])`, which handles counter resets), the queue, live delivery and rate limits. `TestDashboardQueriesKnownMetrics` fails if a panel asks for a metric `internal/metrics` doesn't define: in Grafana a typo only shows as "No data". `increase()` and `rate()` estimate from samples 15 seconds apart and extrapolate to the window's edges, so "jobs finished in the last hour" can read 1.3: an estimate of the counter's growth, not 0.3 of a job. Over longer windows the error shrinks.
- **Quieter logs.** Passing `/healthz` checks (Render, every 5 seconds: about 17,000 lines a day) aren't logged any more; a failing one still is.

## Rate limits: `internal/ratelimit`

Fixed windows: the first attempt for a subject starts a window (15 minutes for login), every attempt adds one, and past the limit the answer is `429` with `Retry-After` until the window ends.

- **One atomic step in Redis.** A Lua script runs `INCR` and, when that created the key, `PEXPIRE`, and returns the count and the time left. A crash between a separate `INCR` and `EXPIRE` would leave a counter that never expires: a lockout nobody meant (`TestCountIsAtomic` checks every counter has an expiry). Keys are `goflow:rl:<rule>:<first 16 bytes of SHA-256 of the subject>`: Redis knows which limit a key is for, not whose email or address it is. Logs name the rule, never the subject.
- **Redis down: count in memory.** The `Limiter` tries Redis on every attempt and falls back to a map in the process, logging the outage once and the recovery once. Logins stay available and limited, though per process and only until the next restart. The map is capped at 100,000 counters, swept of ended windows at most once a second, so inventing new emails can't exhaust memory.
- **Counted before the work.** Login counts the client address first (before the database and bcrypt, and for malformed emails too), then the normalized email, then looks up the user. The email is counted **whether or not it's registered**: counting only real accounts would make `429` mean "this account exists" (FAILURES.md, case 39). Password changes count before the current-password check, tickets before Redis is asked for one.
- **The client's address.** With `GOFLOW_CLIENT_IP_HEADER` empty, the connection's address. On Render it's `CF-Connecting-IP`: Task 37's experiment showed Cloudflare refuses any request that brings its own (`403`, error 1000), while it appends to `X-Forwarded-For`, keeping the client's fake first entry. Behind Render's local proxy the connection address is `[::1]` for everyone, so it can't be used there. A header that's missing or isn't an address falls back to the connection's address. IPv6 counts per /64, the block one customer usually gets.
- **Why every attempt counts, before the password check.** Counting only failures would mean "check the count, run bcrypt, then add one if it failed": fifty guesses sent together would all see "under the limit" before any failure was recorded, the same check-then-act race as idempotency keys (Task 31). Adding one first is a single atomic step in Redis, so the eleventh request is refused however many arrive at once, and a refused one costs no bcrypt time. The price: an honest user's successful logins count too, and ten in 15 minutes is rare.
- **Throttle, never lock.** A lockout lets anyone lock a victim out for good with a few wrong passwords; a window clears by itself.
- **Tests don't share counters.** The e2e servers get their own key prefix (`config.RateLimitPrefix`, not read from the environment), because every e2e test logs in from `127.0.0.1` and counters outlive a test run by 15 minutes.

## Dashboard: `internal/web`

Three files embedded with `//go:embed` and served by the API at `/`, `/app.js` and `/style.css`; every other path is still the API's JSON 404. Plain JavaScript, no framework, no build step, nothing loaded from another site.

- **Same origin as the API.** The page calls `/api/v1/...` on the server that served it, so there are no CORS rules to get wrong.
- **The token lives in `sessionStorage`.** It survives a refresh and is gone when the tab closes. It goes only in the `Authorization` header; the event stream gets a 30-second ticket instead. A `401` from any call logs the page out.
- **A strict Content-Security-Policy on every file:** `default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'self'; frame-ancestors 'none'`. Only `/app.js` runs: no inline scripts, no `onclick=` attributes, no `eval`. `fetch` and `EventSource` can reach only this origin, so even a script that got in couldn't send the token elsewhere. Plus `X-Content-Type-Options: nosniff` and `Referrer-Policy: no-referrer`.
- **Server text is never HTML.** Every element is built with `createElement` and filled with `textContent`. A job titled `<img src=x onerror=alert(1)>` shows as those characters. `TestScriptNeverTurnsTextIntoHTML` fails if `app.js` ever uses `innerHTML` and its relatives, and `TestPageHasNoInlineCode` keeps the HTML free of anything the policy would have to allow (FAILURES.md, case 37).
- **Keeping its copy right.** The page holds jobs in a `Map`. An event replaces a job only if its `version` is higher. After `ready`, `resync`, `resumed` or any reconnect it reloads every page of `GET /api/v1/jobs`: jobs it had before the reload that the database no longer has are removed, and jobs that arrived by event during the reload are kept. Deleted IDs are remembered, so a late `changed` event can't bring one back.
- **Three states for the dot.** `live`, `paused: updates delayed` (the server says events stopped reaching it; the stream stays open) and `offline`/`reconnecting`. An open stream alone never shows `live`: see [Delivery health](#delivery-health).
- **Reconnecting.** `EventSource` retries on its own, but with the same, already used ticket, so the page closes it on any error, gets a new ticket and reconnects, waiting 1 s, then doubling to 30 s. Without Redis (`503` for the ticket) it shows the list from the database and says live updates are unavailable.

Checked in a headless Chromium against a local API with PostgreSQL, Redis and 2 workers: login (and a wrong password), four jobs reaching `completed`/`dead` live, retry and delete, a refresh keeping the session, and an API restart (the page showed `offline`, came back `live`, and showed a job created while it was away). The policy reported no violations.

## CLI: `internal/cli`, `internal/client`, `cmd/goflow`

- `goflow job create | list [--status] | get | delete | retry` sends HTTP requests to `GOFLOW_API_URL` (default `http://localhost:8080`).
- `goflow login` reads the password from the terminal without echo (`golang.org/x/term`) and saves `{api_url, token, expires_at}` in `~/.config/goflow/credentials`: written to a temporary file with mode `0600`, then renamed into place, inside a `0700` directory. The CLI refuses a file that group or others can read, and sends the token only to the API that issued it and only until it expires. `GOFLOW_TOKEN` overrides the file for scripts. `goflow logout` ends the session on the server, then deletes the file.
- Each command has a 90-second deadline (long enough for a sleeping free server to wake), and `Ctrl+C` cancels the request in flight.
- `list` follows `next_cursor` until the last page.
- The client turns a 404 `job not found` back into `job.ErrNotFound`, reports other non-success responses as `*client.APIError`, and reports network failures as ordinary wrapped errors.

## Container image: `Dockerfile`, `compose.yaml`

One image holds the API (its entry point) and the worker binary. It's built in two stages:

```text
golang:1.27-trixie                         gcr.io/distroless/static-debian13:nonroot
  go.mod, go.sum ─► go mod download          /usr/local/bin/api     (entry point)
  cmd/, internal/ ─► go build, CGO off  ───► /usr/local/bin/worker
                                             CA certificates, time zones, user nonroot
```

- **Nothing in the image that GoFlow doesn't run.** The final stage has no compiler, no source, no shell and no package manager: about 2 MiB of base under two static binaries. Less software to patch, and nothing for an attacker to work with.
- **Static, or it doesn't start.** `CGO_ENABLED=0`. The build image has a C compiler, so Go's default would link against the system C library, which the static base doesn't have; the container would fail with `exec /usr/local/bin/api: no such file or directory` (FAILURES.md, case 43). Migrations and the dashboard are embedded (`go:embed`), so the binary needs no other files.
- **CA certificates come with the base.** `sslmode=verify-full` checks Neon's certificate against them; on `scratch` every connection would fail with `x509: certificate signed by unknown authority`.
- **Not root.** The process runs as `nonroot`: a bug in GoFlow can't modify the image's files.
- **No settings or secrets in the image.** No `ARG`, no `ENV GOFLOW_...`: every instruction is kept in the image's history, and Render passes each environment variable as a build argument. Settings arrive when the container starts. `.dockerignore` is an allowlist (`*`, then `go.mod`, `go.sum`, `cmd`, `internal`), so `.env`, `.git`, `bin/` and any stray key file never reach the build. `TestImageIsStaticAndRunsAsNonroot`, `TestImageHoldsNoSettingsOrSecrets`, `TestDockerignoreIsAnAllowlist` and `TestComposePublishesPortsOnLoopbackOnly` read the three files and keep these rules (check.sh can't run Docker).
- **Compose for development.** PostgreSQL 17 (Neon's version), Valkey 8 (Key Value's), the API and three worker containers, ports on `127.0.0.1` only, the API started only once both answer their health checks, and 30 seconds between `SIGTERM` and `SIGKILL` for the pool's 20-second drain. The workers (Task 49) run the API's image with the worker binary as entrypoint (`pull_policy: never`: the api service builds `goflow:dev`, nothing pulls it), two workers each, and scale with `--scale worker=N`; `TestComposeRunsThreeWorkersFromTheAPIsImage` keeps them on the API's image, database and Valkey. The API reaches the database as `postgres`: a single-label name that only exists on Compose's network, which is why `urlcheck` lets it skip TLS, as it already did for Render's private Key Value host.

## Testing

| Layer | Approach |
|---|---|
| `internal/job`, `internal/auth` | Plain unit tests, including the pagination rules and token formats |
| `internal/credentials` | Real files in `t.TempDir()`: modes, refusals, which API gets the token |
| `internal/api` | Handlers with fakes: an adapter over `MemoryStore`, an always-failing store, and a spy that records the context it was given |
| `internal/cli` | A fake API with 2 jobs per page, plus failure and deadline doubles |
| `internal/client` | **Contract tests** against the real `api.NewRouter`, served by `httptest.NewServer` |
| `internal/events` | The hub and the publisher with fakes for Redis: owner-only delivery, slow clients dropped, publishing that never blocks, tickets working once and stored as hashes, and delivery health with each half of the canary's path broken on its own |
| `internal/database` | Real PostgreSQL. `TestMain` migrates one template database per run, and each test copies it. Lease races (a late `Complete` or `Renew` against `Reclaim`) run 30 rounds each. The 77 tests with their own copy run in parallel (Task 53); each copy gets 3 connections, and a test that waits for one fails unless it asked for more (`withConns`) or queues on purpose (`expectConnWaits`, the case 26 deadlock test). The four tests that need 5 to 20 connections at once stay sequential, so their pools never add up with the parallel ones (case 50). On the development laptop the package went from about 17 s to about 5.5 s |
| `cmd/api` | End to end: the real server on `127.0.0.1:0`, including jobs surviving a restart, sign-up, sessions, settings validation, `/metrics` after a job's whole life, live delivery pausing and resuming with Redis behind a TCP proxy the test can cut, three API instances with workers sharing one database and Redis, and two worker pools racing over one lease. Clients come from `testClient`, which closes its idle connections before the server stops; otherwise an unused spare connection makes `Shutdown` wait about 5 s (FAILURES.md, case 48). Test servers' workers wait 1 s per take, not the production 2 s, so stopping one doesn't wait out a 2 s take (case 49). Every test has its own copy of a migrated template database and its own Redis queue keys and events channel, shared by that test's servers only; production keeps `goflow:jobs:*` and `goflow:events` (case 51) |

Everything runs with the race detector (`./scripts/check.sh`). Tests that need PostgreSQL or Redis skip without their URL and still print `ok`, so `check.sh` refuses to run without both (`GOFLOW_QUICK_CHECK=1` runs anyway and says so); `TestCheckRefusesToSkipTheDatabaseTests` runs the script itself to keep that rule (FAILURES.md, case 47). [FAILURES.md](FAILURES.md) records what each safeguard prevents.

## Design decisions

| Decision | Reason |
|---|---|
| Standard library first; dependencies are `pgx`, `go-redis`, `golang.org/x/crypto` (bcrypt) and `golang.org/x/term` | Learn what frameworks hide. Never write your own password hashing |
| Opaque session tokens stored as SHA-256 hashes, not JWTs | Logout and password changes end a session immediately. A copy of the `sessions` table can't be used to log in. Tokens are 256 random bits, so a fast hash is enough; passwords need bcrypt because people pick guessable ones |
| Ownership checked in SQL (`WHERE id = $1 AND user_id = $2`); another user's job is `404` | A forgotten check in a handler can't leak data, and the response doesn't confirm the job exists |
| The same `401` and the same time for every login failure | Response and timing can't reveal which emails have accounts |
| Registration closed by default | A public server doesn't accept strangers until there is a reason to |
| Remote database only over TLS (`verify-full`); this machine and single-label private-network names excepted | Passwords, hashes and session hashes never cross the internet in plain text. A name without a dot (Compose's `postgres`) can't be resolved by public DNS, so the connection stays on a private network; Neon's host always needs TLS |
| One image: two build stages, distroless static, non-root, static binaries | Only what runs is shipped; CA certificates for Neon included; no shell for an attacker; the same image locally and on Render (`runtime: docker`) |
| Redis as the queue, next to PostgreSQL | Workers wait on `BLMOVE` instead of polling the database, which also lets a sleeping free database stay asleep. The cost: a second system to run and secure, and the dual-write problem, which the outbox exists to solve. PostgreSQL alone (`FOR UPDATE SKIP LOCKED`) would have avoided it; Redis was chosen to learn the queue patterns |
| Leases with a token on every write; PostgreSQL's clock decides expiry, Redis only keeps deadlines to know when to ask | Several worker processes can share the work without resetting each other's jobs, and a worker whose lease ran out can't overwrite the next one's result. Asking PostgreSQL only for passed deadlines keeps a free database asleep |
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
| Server-Sent Events, not WebSocket | Updates only flow from server to browser. SSE is a plain HTTP response (`net/http` only), and browsers reconnect on their own. WebSocket would add a dependency and two-way features nobody uses |
| Redis Pub/Sub for events, not PostgreSQL `LISTEN/NOTIFY` | Neon drops `LISTEN` registrations whenever the database sleeps (every 5 idle minutes on the free plan). Pub/Sub works whether workers run in the API or in `cmd/worker` |
| Events are published after commit, in the background, and may be lost | They are hints. The client's snapshot after `ready`, `resync`, and reloading after a reconnect cover every loss; PostgreSQL never depends on them |
| A per-job `version`, raised by every `UPDATE`, orders a job's changes | Row locks make it follow commit order. Timestamps from `now()` follow transaction start, can tie, and depend on the database's clock |
| Dashboard: plain JavaScript embedded in the API, token in `sessionStorage`, strict CSP | No build step or third-party code to trust; same origin; the token survives a refresh but not the tab. The policy and text-only rendering stand between an injected title and the token |
| Prometheus metrics behind a token; on Render read by hand, locally collected by Prometheus (compose profile `monitoring`) | The standard format with labels and histograms; no hosted collector, because polling would keep the free instance awake all month |
| Rate limits in fixed windows, counted in Redis with an in-memory fallback; `429` + `Retry-After`, no lockouts | Shared across instances and restarts; still limited when Redis is down; nobody can lock another user out for good |
| Client address from `CF-Connecting-IP` on Render, chosen by experiment | Cloudflare refuses requests that send it; `X-Forwarded-For` keeps whatever the client put first |
| Delivery health by canary: the API publishes to itself every 2 s; 8 s without one back is a pause | Only a message that came back proves the path works, and it doesn't depend on job activity. 10 s to notice is quick for a person watching and slow enough that a reconnect go-redis repairs in 100 ms is never reported |
| Single-use 30-second tickets for streams | Keeps the 24-hour session token out of URLs, which end up in logs and history |
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
- On Render the pool runs inside the single free API instance. Several processes are safe (leases, Task 48) but only run locally so far, in the Compose stack's three worker containers.
- A worker frozen for longer than its lease (30 s) can still be *running* its handler while another worker runs the same job: fencing stops it from recording anything, not from working. Handlers with outside effects must be idempotent ([Handler contract](#handler-contract)).
- Jobs reach Redis at least once, and a handler can run more than once. Handlers must follow the [handler contract](#handler-contract); nothing enforces it.
- When Key Value loses its data, retries and queued jobs come back through the hourly reconciler: up to about 70 minutes late.
- `last_error` is stored but not shown to users yet: `goflow job get` on a dead job doesn't say why it died.
- Idempotency keys never expire on their own; they're deleted with their job.
- A session ending (logout, password change) doesn't close streams it opened: they run until they drop or the server restarts. A ticket can't be issued without a live session, and a stream only carries its owner's job changes.
- The dashboard keeps the session token where its own script can read it (`sessionStorage`). The policy and text-only rendering make injected script very unlikely to run, but if one ever did, it could act as the user in that tab until the token expires. An `HttpOnly` cookie would hide the token from scripts, at the cost of CSRF protection on every request (considered for a later phase).
- **The dot needs 8–10 seconds to notice a pause.** Until then it still says `live` over a list that has stopped moving. Nothing is lost either way: the page reloads from PostgreSQL on `resumed`. A shorter timeout would report brief network hiccups as pauses.
- The dashboard has no sign-up form: accounts are created with registration open, as in [DEPLOYMENT.md](DEPLOYMENT.md).
- Plain `redis://`, and a database without TLS, are accepted for single-label private-network names (Render's internal Key Value host, Compose's `postgres`): that traffic is unencrypted inside the private network, so GoFlow trusts its isolation. A DNS search domain could in theory turn such a name into a public one; Compose's own DNS answers its service names first.
- **The per-email login limit can be used against its owner.** Anyone who knows your email can send 10 wrong passwords and make you wait up to 15 minutes to log in. That's the price of stopping distributed guessing of one account; a lockout would have been worse (it never ends by itself). Possible softening later: let a client that has logged in successfully from the same address before skip the per-email limit.
- **While Redis is down, limits are per process and reset on restart.** On Render's single free instance that means a restart (every 15 idle minutes) gives a fresh allowance until Redis is back.
- After the bcrypt cost changes, response time can tell old accounts from unknown emails until each user logs in once.
- A failed readiness check gives two log lines: the cause (`readiness check failed`, with the dependency) and the `503` request. Both carry the request ID. Kept as it is: merging them would need the request logger to know why a handler failed.
- **The image tests read text.** `TestImageHoldsNoSettingsOrSecrets` catches `ARG` and `ENV GOFLOW_...`, and `.dockerignore` keeps everything but `go.mod`, `go.sum`, `cmd` and `internal` out of the build. It can't catch a secret under another name (`ENV DB_PASSWORD=...`), one written by a `RUN` step, or a secret file placed inside `cmd/` or `internal/`, which the allowlist lets in. Scanning the built image for secrets (and never keeping key files in the repo folder) would close that gap.
- On Render, `/metrics` is read by hand (`curl` with the token), not collected; only the local stack has Prometheus. Counters start from zero at every restart, and a free instance restarts after 15 idle minutes, so they show "since this instance started", not history. A collector would keep the instance awake around the clock.
- The CLI keeps one saved login at a time: logging in to another API replaces it.
- Hosting is free tier: the first request after idle waits for the server and database to wake ([DEPLOYMENT.md](DEPLOYMENT.md)).
