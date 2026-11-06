# Failures

Every safeguard in GoFlow exists because removing it breaks something. This file records, for each one: what it protects, the change that removes it, what catches the problem, and the output observed when it was removed on purpose.

Each case was reproduced on a real machine (Arch Linux, Go 1.27, PostgreSQL 17) during development. To reproduce one, commit your work first, apply the change, run the command, then restore with `git checkout -- <file>`.

---

## 1. Titles are measured in characters, not bytes

- **Safeguard:** `job.New` uses `utf8.RuneCountInString(title)` for the 200-character limit.
- **Removed:** `len(title)`, which counts bytes.
- **Caught by:** `TestNew/200_multi-byte_characters`
- **Observed:**
  ```text
  --- FAIL: TestNew/200_multi-byte_characters
      error = job: title exceeds 200 characters, want <nil>
  ```
- **Why:** `"é"` is 2 bytes in UTF-8, so 200 of them measure 400 with `len`. Users writing in French, Yoruba with tone marks, or with emoji would be rejected well before 200 characters.

## 2. An empty job list is `[]`, never `null`

- **Safeguard:** `List` builds its result with `make([]*Job, 0, n)`.
- **Removed:** `var out []*Job` (a nil slice).
- **Caught by:** `TestMemoryStoreListEmptyIsNotNil`
- **Observed:**
  ```text
  List() returned nil, want empty slice
  ```
- **Why:** `encoding/json` encodes a nil slice as `null`. A client calling `jobs.map(...)` on an empty list would crash. Only the empty case fails: `append` works on nil slices, so every non-empty test still passed.

## 3. A missing data file means "no jobs yet"

- **Safeguard:** `Load` treats `errors.Is(err, fs.ErrNotExist)` as an empty store.
- **Removed:** the three-line `fs.ErrNotExist` check.
- **Caught by:** `TestLoadMissingFileReturnsEmptyStore`
- **Observed:**
  ```text
  Load: job: read /tmp/.../does-not-exist.json: open /tmp/.../does-not-exist.json: no such file or directory
  ```
- **Why:** on a fresh install, the very first `goflow job list` would fail because no jobs had been saved yet.
- **Note:** this break was accidentally committed and merged (PR #3). `main` stopped compiling until PR #4 restored the check. Lesson: commit exactly what was tested.

## 4. Unknown JSON fields are rejected

- **Safeguard:** `decodeJSON` calls `dec.DisallowUnknownFields()`.
- **Removed:** that line.
- **Caught by:** `TestCreateJobRequestValidation/unknown_field`
- **Observed:**
  ```text
  status = 201, want 400
  ```
- **Why:** `{"title":"x","priority":5}` was accepted and `priority` silently dropped. The client believed it had set a priority.

## 5. Shutdown waits for in-flight requests

- **Safeguard:** `srv.Shutdown(ctx)` with a 10-second limit.
- **Removed:** `srv.Close()`.
- **Caught by:** no automated test yet. Reproduced manually with a temporary 5-second `/slow` endpoint and `SIGINT` sent 1 second into the request.
- **Observed:**

  | | `Shutdown` | `Close` |
  |---|---|---|
  | Gap between signal and stop | 4.2 s | 0 ms |
  | Client result | `200 {"status":"finally"}`, `Connection: close` | `curl: (52) Empty reply from server` |

- **Why:** every deploy sends `SIGTERM`. With `Close`, every request in progress at that moment fails.

## 6. The in-memory store is locked

- **Safeguard:** `MemoryStore` guards its map with `sync.RWMutex`.
- **Removed:** the lock (the store as originally written in Task 2).
- **Caught by:** `TestMemoryStoreConcurrentAccess`, `TestConcurrentRequests`
- **Observed** without the race detector, the process crashed:
  ```text
  fatal error: concurrent map writes
  ... (*MemoryStore).Add ... store.go:35
  fatal error: concurrent map iteration and map write
  ... (*MemoryStore).List ... store.go:51
  ```
  With `-race`:
  ```text
  WARNING: DATA RACE
  Read at ... by goroutine 57:  (*MemoryStore).Add() store.go:32
  Previous write at ... by goroutine 43:  (*MemoryStore).Add() store.go:35
  ```
- **Why:** `net/http` runs every request in its own goroutine. Line 32 is the duplicate-ID check and line 35 the write: a check-then-act race. A `fatal error` can't be recovered, so one bad moment takes the whole server down.

## 7. Writers take the exclusive lock

- **Safeguard:** `Add` and `Delete` use `Lock`. `Get` and `List` use `RLock`.
- **Removed:** `RLock` in `Add`.
- **Caught by:** `TestMemoryStoreConcurrentAccess` with `-race`
- **Observed:** the same race as case 6, at the same two lines.
- **Why:** `RLock` allows many holders at once. It promises that every holder only reads.

## 8. A request that is never cancelled never ends

- **Safeguard:** the end-to-end test calls `cancel()` before waiting for the server to stop.
- **Removed:** that `cancel()` call.
- **Caught by:** `TestServerEndToEnd`, through its 15-second guard
- **Observed:**
  ```text
  --- FAIL: TestServerEndToEnd (15.01s)
      server did not shut down within 15s
  ```
- **Why:** `run` waits for its context to be cancelled. Without the `time.After` guard, the test would have hung until `go test`'s 10-minute default timeout.

## 9. The PostgreSQL driver must be imported

- **Safeguard:** `import _ "github.com/jackc/pgx/v5/stdlib"` in `internal/database`.
- **Removed:** that import.
- **Caught by:** `TestOpenConnectsAndConfiguresPool`, **not** the compiler
- **Observed:**
  ```text
  BUILD STILL WORKS
  Open: database: open: sql: unknown driver "pgx" (forgotten import?)
  ```
- **Why:** `sql.Open("pgx", ...)` looks the driver up by name at runtime. The blank import registers that name. To the compiler, `"pgx"` is only a string.

## 10. Migrations hold an advisory lock

- **Safeguard:** each migration runs in a transaction holding `pg_advisory_xact_lock`.
- **Removed:** the lock (replaced with `SELECT $1::bigint`).
- **Caught by:** `TestMigrateConcurrently` (5 concurrent callers), failing 5 out of 5 runs
- **Observed:**
  ```text
  apply migration 0001_create_jobs.sql: ERROR: duplicate key value violates unique constraint "pg_type_typname_nsp_index" (SQLSTATE 23505)
  apply migration 0001_create_jobs.sql: ERROR: relation "jobs" already exists (SQLSTATE 42P07)
  ```
- **Why:** every caller saw "migration 1 not applied" and ran `CREATE TABLE jobs`. That's the same check-then-act race as case 6, now across database connections, where a Go mutex can't help. PostgreSQL's own catalog prevented duplicate tables, but only by failing 4 of the 5 callers.

## 11. Timestamps are truncated to microseconds

- **Safeguard:** `job.New` uses `time.Now().UTC().Truncate(time.Microsecond)`.
- **Removed:** the `Truncate` call.
- **Caught by:** `TestJobStoreAddAndGetRoundTrip` and `TestNewTruncatesToMicroseconds`
- **Observed:**
  ```text
  timestamps changed on round trip:
   got  2026-10-24 15:45:28.598097 +0000 UTC
   want 2026-10-24 15:45:28.598097452 +0000 UTC

  CreatedAt has 62 leftover nanoseconds, want 0
  ```
- **Why:** Go keeps nanoseconds and PostgreSQL keeps microseconds, so a stored job no longer equaled itself. PostgreSQL also rounds the extra digits, which can push a time later. Truncating once in Go leaves nothing to round.

## 12. The request ID travels in the context

- **Safeguard:** `withRequestID` passes `r.WithContext(ctx)` to the next handler.
- **Removed:** passing the original `r`.
- **Caught by:** `TestLogRequestsWritesOneLinePerRequest`. All `TestRequestID...` tests still passed.
- **Observed:**
  ```text
  --- FAIL: TestLogRequestsWritesOneLinePerRequest
      log line missing "request_id":"test-id"
      log: {"level":"INFO","msg":"request","request_id":"","method":"GET","path":"/nope","status":404,...}
  ```
- **Why:** the response header is written straight to `w`, so the client still received an ID. The log reads the ID from the request's context, and `context.WithValue` returns a new context that only the new request carries. A user would report an ID that appears nowhere in the logs.

## 13. Storage calls inherit the request's context

- **Safeguard:** `dbContext` derives the storage context from `r.Context()` with a 3-second timeout.
- **Removed:** `context.WithTimeout(context.Background(), dbTimeout)`.
- **Caught by:** `TestStoreReceivesRequestContextWithDeadline`. Its deadline check still passed.
- **Observed:**
  ```text
  --- FAIL: TestStoreReceivesRequestContextWithDeadline
      after the client disconnected, store ctx.Err() = <nil>, want context.Canceled
  ```
- **Why:** every request still succeeded and the 3-second limit still applied, but a client that disconnected no longer cancelled its query. Slow queries would keep running and holding pool connections for responses nobody reads.

## 14. The template database is closed before it's copied

- **Safeguard:** `createTemplate` calls `tpl.Close()` after migrating the template database.
- **Removed:** that call.
- **Caught by:** every test that uses `newMigratedTestDB`
- **Observed:**
  ```text
  --- FAIL: TestJobStoreAddAndGetRoundTrip (5.03s)
      create test database: ERROR: source database "goflow_tpl_053c99f0476b" is being accessed by other users (SQLSTATE 55006)
  ```
  Every affected test took 5 seconds to fail. The run took 36.6 s instead of about 2 s.
- **Why:** PostgreSQL won't copy a database that has open connections. It waits up to 5 seconds for them to close before failing. The template's pool kept idle connections open, so every copy waited the full 5 seconds and then failed. The leak first showed up as slowness, not as an error.

- **Template databases as a speed-up:** measured over 5 runs each, the template version averaged 1.76 s against 2.06 s, but the ranges overlapped almost completely. With one migration, `CREATE DATABASE` dominates, and every `CREATE DATABASE` already copies a template. Kept for when migrations grow, not for speed today.

## 15. Pages fetch one extra row

- **Safeguard:** `JobStore.List` queries `LIMIT limit + 1`. The extra row only proves that another page exists.
- **Removed:** `LIMIT limit`.
- **Caught by:** `TestJobStoreListPaginates`
- **Observed:**
  ```text
  pages = 1, want 3 (2 + 2 + 1)
  paged IDs = [1075f347... 4ef350e5...],
         want [1075f347... 4ef350e5... 6af9be0f... 16b09a43... 85274e9d...]
  ```
- **Why:** a full first page looked exactly like a last page, so no `next_cursor` was returned. Clients stopped after page 1 and three of five jobs were never shown, with no error anywhere.

- **Since Task 15:** the JSON file store was removed when the CLI became an API client. The lesson stands.

## 16. The CLI moves to the next page

- **Safeguard:** `runList` sets `cursor = next` after each page.
- **Removed:** that line.
- **Caught by:** `TestListShowsEveryPage`, but only because the test run had a time limit
- **Observed:**
  ```text
  panic: test timed out after 5s
          running tests:
                  TestListShowsEveryPage (5s)
  ```
- **Why:** the CLI asked for page 1 forever. Nothing failed: every request succeeded. Run with `go test -timeout 5s`, Go stopped it and named the stuck test. With the default 10-minute timeout, the same bug looks like a frozen terminal or a CI job that never finishes.

## 17. Passwords are hashed with a salted, slow hash

- **Safeguard:** `Hasher.Hash` uses `bcrypt.GenerateFromPassword`.
- **Removed:** an unsalted SHA-256 of the password (`sha256.Sum256`, hex-encoded).
- **Caught by:** `TestHashIsSalted`, `TestHashUsesCost` and `TestHashAndCompare`
- **Observed:**
  ```text
  --- FAIL: TestHashAndCompare (0.00s)
      password_test.go:46: Compare(right password) = auth: compare password: crypto/bcrypt: bcrypt hashes must start with '$', but hashedSecret started with '9', want nil
  --- FAIL: TestHashIsSalted (0.00s)
      password_test.go:63: two hashes of the same password are equal: no salt
  --- FAIL: TestHashUsesCost (0.00s)
      password_test.go:74: crypto/bcrypt: bcrypt hashes must start with '$', but hashedSecret started with 'f'
  ```
- **Why:** SHA-256 gives the same output for the same input on every machine. `echo -n "same password twice" | sha256sum` printed `f0f2b492...` twice. With a leaked users table, an attacker hashes millions of common passwords once and looks up every account. bcrypt stores a random salt inside each hash, so two users with the same password get different hashes. Its cost setting also makes every guess slow: measured with `go test -bench` on an i5-8265U, cost 12 took 446 ms per hash and cost 4 took 1.8 ms. SHA-256 has no cost to raise.

## 18. A taken email is told apart by the constraint name

- **Safeguard:** `UserStore.Create` returns `auth.ErrEmailTaken` only when the unique violation comes from `users_email_key`.
- **Removed:** the `pgErr.ConstraintName == usersEmailKey` check, so every `23505` became "email taken".
- **Caught by:** `TestUserStoreDuplicateIDIsNotEmailTaken`
- **Observed:**
  ```text
  --- FAIL: TestUserStoreDuplicateIDIsNotEmailTaken (0.18s)
      users_test.go:83: err = auth: email already registered, want a plain error: the email is free, the ID is not
  ```
- **Why:** the `users` table has two unique constraints, the primary key on `id` and `UNIQUE` on `email`, and both raise SQLSTATE `23505`. Without the name check, a duplicate ID reaches the client as `409 email is already registered` for an email nobody has used. Random 128-bit IDs practically never collide, so this would only appear through a bug, such as code that reuses an ID. The wrong message would then send the debugging in the wrong direction.

## 19. The bcrypt cost has a floor

- **Safeguard:** `loadConfig` refuses a `GOFLOW_BCRYPT_COST` below 10.
- **Removed:** the `cfg.BcryptCost < minBcryptCost ||` half of the range check, so only "above 14" was refused.
- **Caught by:** `TestLoadConfigRejectsBadSettings` (3 subtests)
- **Observed:**
  ```text
  --- FAIL: TestLoadConfigRejectsBadSettings/cost_below_10 (0.00s)
      config_test.go:69: loadConfig succeeded, want an error
  --- FAIL: TestLoadConfigRejectsBadSettings/bcrypt_minimum (0.00s)
      config_test.go:69: loadConfig succeeded, want an error
  --- FAIL: TestLoadConfigRejectsBadSettings/every_problem_reported (0.00s)
      config_test.go:73: error "GOFLOW_DATABASE_URL is required" does not mention "got 3"
  ```
- **Why:** without the floor, a test setting copied into production (`GOFLOW_BCRYPT_COST=4`) starts the server normally, and every password hashed from then on is about 240 times cheaper to crack: 1.8 ms per guess instead of 446 ms on the development laptop. Nothing looks wrong. Logins just get faster. The bad hashes also stay weak after the setting is fixed, until each user's password is hashed again.

## 20. Unknown emails take as long as wrong passwords

- **Safeguard:** when an email isn't registered, login still compares the password against a dummy bcrypt hash made at the server's cost.
- **Removed:** the `h.hasher.Compare(h.dummy.get(h.hasher), req.Password)` line.
- **Caught by:** `TestLoginTimingHidesUnknownEmails`
- **Observed:**
  ```text
  --- FAIL: TestLoginTimingHidesUnknownEmails (0.06s)
      login_test.go:189: unknown email took 45.552µs, wrong password 28.189995ms: the difference reveals which emails exist
  ```
- **Why:** the error message stayed identical, but the response time didn't. An unknown email answered in 46 µs, a registered one in 28 ms (cost 8 in the test; about half a second at cost 12). Anyone can time a list of emails with curl, learn who has an account, and then aim password guessing or phishing at exactly those people. Same message is not enough; same work is needed.

## 21. Every job query filters by owner

- **Safeguard:** `JobStore.Get` puts the caller's user ID in the WHERE clause: `WHERE id = $1 AND user_id = $2`.
- **Removed:** the owner filter, leaving `WHERE id = $1`.
- **Caught by:** `TestJobStoreKeepsUsersApart` and `TestServerEndToEnd`. The API handler tests still passed.
- **Observed:**
  ```text
  --- FAIL: TestJobStoreKeepsUsersApart
      jobs_test.go:135: bob reading alice's job: err = <nil>, want job.ErrNotFound
  --- FAIL: TestServerEndToEnd
      main_test.go:172: get as another user: status 200, body {"id":"…","title":"e2e job",…}
  ```
- **Why:** the route still required a session, so only signed-in users got through. But being signed in is authentication, not authorization: any signed-in user could read any job by its ID. This is number one on OWASP's API Security list, Broken Object Level Authorization. Random 128-bit IDs are hard to guess, but IDs leak through links, logs and screenshots, so they are not a lock. Because the owner check lives in the SQL, there is one place to get it right, and no handler can forget it.

## 22. A password change ends every other session

- **Safeguard:** the change-password handler calls `UserStore.ChangePassword`, which stores the new hash and deletes the user's other sessions in one transaction.
- **Removed:** the handler called the older `UpdatePasswordHash` instead. Both methods exist, so this compiles and looks reasonable.
- **Caught by:** `TestChangePassword` and `TestAccountSecurityEndToEnd`. The database tests still passed.
- **Observed:**
  ```text
  --- FAIL: TestChangePassword (0.01s)
      account_test.go:64: sessions_ended = 0, want 1 (the tablet)
      account_test.go:71: another session survived the password change: status 200
  --- FAIL: TestAccountSecurityEndToEnd (0.05s)
      session_test.go:106: tablet after password change: status 200, want 401
  ```
- **Why:** the password changed and the old one stopped working, so from the user's side everything looked fine. But people change a password because they think someone else got in, and that someone was still signed in on the "stolen tablet" with a valid session for up to 24 hours. A password change has to end the other sessions, or it only locks the front door while the intruder is already inside.

## 23. The CLI only sends a saved token to the API that issued it

- **Safeguard:** `credentials.File.TokenFor` returns the saved token only when `GOFLOW_API_URL` matches the API it came from.
- **Removed:** the `c.APIURL != apiURL` check, so only the expiry was checked.
- **Caught by:** `TestTokenFor`
- **Observed:**
  ```text
  --- FAIL: TestTokenFor
      credentials_test.go:150: the token was offered to a different API
  ```
- **Why:** `GOFLOW_API_URL` is just an environment variable. A shared script, a copied `.env` file or a typo can point it at another server, and the CLI would then send your session token to that server in the first request. Whoever runs it could act as you until the session expires. Remembering which API issued the token, and sending it nowhere else, keeps a wrong setting from turning into a stolen session.

## 24. A remote database needs TLS

- **Safeguard:** `loadConfig` refuses a `GOFLOW_DATABASE_URL` on another machine unless it asks for TLS (`sslmode=verify-full`, `verify-ca` or `require`).
- **Removed:** the `checkDatabaseTLS` call.
- **Caught by:** `TestLoadConfigRequiresTLSForRemoteDatabases`
- **Observed:**
  ```text
  --- FAIL: TestLoadConfigRequiresTLSForRemoteDatabases
      config_test.go:134: postgres://u:pw@db.example.com/db: err = <nil>, want ok = false
      config_test.go:134: postgres://u:pw@db.example.com/db?sslmode=disable: err = <nil>, want ok = false
      config_test.go:134: postgres://u:pw@db.example.com/db?sslmode=prefer: err = <nil>, want ok = false
      config_test.go:134: not a url: err = <nil>, want ok = false
  ```
- **Why:** without TLS, the database password and every row, including password hashes and session token hashes, cross the internet in plain text. `sslmode=prefer` looks safe but quietly falls back to plain text if TLS fails. Refusing to start is louder than a silent downgrade: a wrong URL is found at deploy time, not after the data has leaked.


## 25. Only a real queue goes into the interface

- **Safeguard:** `run` puts the queue into `api.Deps` only when there is one: `if q != nil { deps.Queue = q }`.
- **Removed:** `deps.Queue = q` on every start, so a server without `GOFLOW_REDIS_URL` stores a nil `*queue.Queue` in the `JobQueue` interface.
- **Caught by:** `TestServerEndToEnd` and `TestJobsSurviveRestart`, which run without Redis. `TestCreatedJobsReachTheQueue` still passed (it has Redis), and so did every `internal/api` test (they build `Deps` with a fake queue or none).
- **Observed:**
  ```text
  --- FAIL: TestServerEndToEnd (0.05s)
      main_test.go:177: readyz: status 500, body {"error":"internal error"}
  --- FAIL: TestJobsSurviveRestart (0.09s)
      main_test.go:210: create: status 500, body {"error":"internal error"}
  ```
- **Why:** an interface value holds a type and a value. A nil `*queue.Queue` stored in it is `(*queue.Queue, nil)`, which is not `== nil`, so the handler's `h.queue != nil` check passed and `PingContext` and `Enqueue` ran on a nil pointer. The panic was recovered into a `500`. Every server without Redis, including the hosted API on Render, would have failed every readiness check and every new job. Render checks `/healthz`, which doesn't touch the queue, so the deploy would still have shown **Live**.

## 26. The job and its outbox row share one transaction

- **Safeguard:** `JobStore.Add` inserts the job with `tx.ExecContext`, the same transaction as its `job_outbox` row.
- **Removed:** `s.db.ExecContext` for the job insert, so the job is committed on its own before the outbox insert runs.
- **Caught by:** `TestAddIsAllOrNothing`, and, differently, `TestJobStoreConcurrentAdds`.
- **Observed:**
  ```text
  --- FAIL: TestAddIsAllOrNothing (0.10s)
      outbox_test.go:76: Get after the failed Add: err = <nil>, want job.ErrNotFound: the job was saved without its outbox row

  go test -timeout 30s -run TestJobStoreConcurrentAdds ./internal/database/
  panic: test timed out after 30s
          running tests:
                  TestJobStoreConcurrentAdds (30s)
  ```
- **Why:** one word brought back two bugs. First, the dual write: with the outbox insert failing, the job was already committed and would never be queued. Second, a connection-pool deadlock: each `Add` held one pooled connection for its open transaction and asked the pool for a second one for the job insert. The pool has 10 connections; with enough concurrent `Add` calls, every connection is held by a transaction waiting for another, and nobody ever finishes. Inside a transaction, every statement has to go through `tx`. The concurrent test only finished because of `-timeout`.

## 27. A taken job ID moves, it isn't popped

- **Safeguard:** `queue.Take` uses `BLMOVE`, which moves the ID from `goflow:jobs:pending` to `goflow:jobs:processing` in one step. Only `Ack`, after the outcome is saved, removes it.
- **Removed:** `BRPOP` instead, which takes the ID out of Redis entirely.
- **Caught by:** `TestTakeKeepsTheIDUntilAck` and `TestRecoverPutsUnfinishedIDsFirst`. Every `internal/worker` test still passed: they use an in-memory queue.
- **Observed:**
  ```text
  --- FAIL: TestTakeKeepsTheIDUntilAck (0.01s)
      queue_test.go:128: processing list = [], want [older]: a taken ID must not exist only in memory
  --- FAIL: TestRecoverPutsUnfinishedIDsFirst (0.01s)
      queue_test.go:166: Recover = 0, <nil>; want 2, nil
  ```
- **Why:** with `BRPOP`, between taking an ID and finishing the job the ID exists only in the worker's memory. A crash, a deploy or Ctrl+C in that window loses it: the job stays `processing` in PostgreSQL forever and no list holds its ID, so nothing will ever run or retry it. It's the dual-write problem again, on the consumer side. Moving the ID keeps it in exactly one Redis list at every moment.

## 28. Only one worker can claim a job

- **Safeguard:** `WorkStore.Claim` changes a job to `processing` only from `pending` or `queued`: `WHERE id = $1 AND status IN ('pending', 'queued')`.
- **Removed:** `'processing'` added back to that list, as it was in Task 24, where it let a single worker resume a job after a crash.
- **Caught by:** `TestClaimRefusesAProcessingJob`, and `TestClaimHasExactlyOneWinner`, which releases two goroutines at the same moment against real PostgreSQL, 50 rounds.
- **Observed:**
  ```text
  --- FAIL: TestClaimRefusesAProcessingJob (0.11s)
      work_test.go:71: second Claim of a processing job = true, want false: the job would run twice
  --- FAIL: TestClaimHasExactlyOneWinner (0.25s)
      work_test.go:121: round 0: 2 goroutines won the claim, want exactly 1
  ```
- **Why:** with a pool, a duplicate ID (at-least-once delivery) can be taken by two workers at once. PostgreSQL locks the row for the first `UPDATE`; the second waits, then re-checks the `WHERE` clause against the committed row. If `processing` still matches, the second claim also wins, and the job runs twice at the same time. The race failed in the very first round. A Go mutex couldn't prevent this across processes; the condition in the `UPDATE` does. Resuming a crashed job now happens once, at startup (`ResetProcessing`), instead of in every claim.

## 29. SIGTERM stops new claims, not running jobs

- **Safeguard:** `Pool.Run` gives running jobs their own context, `context.WithCancel(context.WithoutCancel(ctx))`. `SIGTERM` cancels only `ctx`, which stops workers from taking new jobs; running jobs are cancelled only after `DrainTimeout`.
- **Removed:** jobs run under the stop context (`p.process(stop, ...)`), so `SIGTERM` reaches them directly.
- **Caught by:** `TestStopLetsTheActiveJobFinish` and `TestDrainTimeoutCancelsAndKeepsTheJob`.
- **Observed:**
  ```text
  --- FAIL: TestStopLetsTheActiveJobFinish (0.02s)
      worker_test.go:293: the active job's context was cancelled (context canceled): SIGTERM must not cancel running jobs
  --- FAIL: TestDrainTimeoutCancelsAndKeepsTheJob (0.00s)
      worker_test.go:320: Run returned after 68.307µs, before the 50ms drain timeout
  ```
- **Why:** every deploy sends `SIGTERM`. With one context, every running job would be cut off the moment a deploy started, even one a second from finishing, and then run again from the start after the restart: wasted work, and twice the side effects for anything not idempotent. The drain timeout never applied: `Run` returned 68 µs after the stop. Stopping new work and stopping current work are different decisions, so they need different contexts.

## 30. The relay doesn't poll while healthy

- **Safeguard:** the relay checks the outbox at start, when woken by a new job, and on a retry timer only after a failed pass (1 s, doubling to 5 minutes). While all is well it sets no timer, so it doesn't query the database between jobs.
- **Removed:** a timer in the healthy branch too, so the relay checks the outbox again after every pass, as the 30-second ticker of Task 23 did, only faster.
- **Caught by:** `TestRelayDoesNotPollWhileHealthy`, which counts outbox passes over 100 ms with nothing to do.
- **Observed:**
  ```text
  --- FAIL: TestRelayDoesNotPollWhileHealthy (0.10s)
      relay_test.go:189: relay made 84 passes with nothing to do, want 1 (the one at start)
  ```
- **Why:** every pass is a database query, and nothing else catches this: jobs still arrive, every other test passes, and the cost shows up only on the bill. Neon's free compute suspends after 5 idle minutes and has 100 compute-hours a month. Even Task 23's 30-second tick never lets it reach 5 idle minutes: at the smallest size that is about 180 compute-hours a month, so the database would be suspended for the rest of the month after about 2.5 weeks. Work that only matters after a failure belongs on a retry timer; work that covers lost data (the reconciler) can run hourly.

## 31. The API starts when Redis is down

- **Safeguard:** the API creates its Redis client with `queue.New`, which doesn't connect, and treats a failed startup ping as a warning (`queue not reachable yet`). The relay, the reconciler and the worker pool all retry on their own.
- **Removed:** returning the ping error from `run`, so the API exits when Redis doesn't answer at startup, as it did before Task 28.
- **Caught by:** `TestAPIStartsWhenRedisIsDown`, which starts the API with `GOFLOW_REDIS_URL` pointing at a port where nothing listens.
- **Observed:**
  ```text
  --- FAIL: TestAPIStartsWhenRedisIsDown (1.74s)
      queue_test.go:113: server failed to start: queue: ping: dial tcp 127.0.0.1:1: connect: connection refused
  ```
- **Why:** this is the failed Render deploy of 3 November, reproduced on a laptop. A Blueprint sync created the Key Value and redeployed the API at the same moment; the new API pinged Redis before it accepted connections and exited, so the deploy failed. Exiting protected nothing: job creation never needed Redis (the outbox), and every Redis user already retried. A dependency the program can work without shouldn't be able to stop it from starting. The database is different: without it the API can't serve anything, so it still exits.

## 32. A retry is recorded in PostgreSQL before Redis schedules it

- **Safeguard:** `Pool.retry` calls `Store.Retry` first (the job becomes `retrying` with `next_attempt_at` and `last_error`, committed), and only then `Queue.Schedule` (the ID goes into the delayed set). If the PostgreSQL write fails, nothing is scheduled and the ID stays in the processing list.
- **Removed:** `Queue.Schedule` called before `Store.Retry`.
- **Caught by:** `TestRetryIsRecordedBeforeItIsScheduled`, which records the order of the two calls and then makes the PostgreSQL write fail.
- **Observed:**
  ```text
  --- FAIL: TestRetryIsRecordedBeforeItIsScheduled (0.00s)
      worker_test.go:424: calls = [redis:schedule ok postgres:retry ok redis:schedule ok], want [postgres:retry ok redis:schedule ok]: PostgreSQL first, then Redis
  ```
- **Why:** the same two-systems problem as the dual write, now inside the worker. Scheduling first means that if the PostgreSQL write then fails, Redis holds a retry the source of truth never recorded: the job is still `processing` in PostgreSQL while its ID will come back from the delayed set. Recording first means the worst case is the reverse, a retry PostgreSQL knows about but Redis doesn't, and that one is safe: the reconciler finds `retrying` jobs that are overdue and sends them again. Write to the system of record first; let the other system be the one that can be repaired.

## 33. Every attempt has a deadline

- **Safeguard:** `Pool.runHandler` runs the handler under `context.WithTimeout(ctx, p.JobTimeout)` (15 s in production). When the deadline passes, the attempt fails with `timed out after 15s` and is retried like any other failure.
- **Removed:** the deadline. The handler ran with the pool's job context only, which ends at shutdown and never before.
- **Caught by:** `TestJobTimeoutRetriesAHangingJob`: one worker, a job that waits for its context, and a second job behind it.
- **Observed:**
  ```text
  --- FAIL: TestJobTimeoutRetriesAHangingJob (6.00s)
      worker_test.go:659: after 5s: stuck timed out and retrying, next completed
  ```
- **Why:** a handler waiting on something that never answers (a server that accepts the connection and then says nothing) holds its worker forever. On Render there are 2 workers, so 2 such jobs stop every other job: the test shows it with 1 worker, where `next` never ran. Nothing failed, so nothing was retried or logged, and the job stayed `processing` until the next restart. With the deadline, a hung job uses up its attempts and ends `dead`: live on Render, `hang` went `retrying` after 15 s each time and was `dead` 3/3 (DEPLOYMENT.md). A deadline only works if the handler passes `ctx` on: Go can't stop a goroutine from outside.

## 34. A reused Idempotency-Key returns the first job

- **Safeguard:** when `JobStore.Add` returns `job.ErrDuplicateKey`, `createJob` calls `replayCreate`: it reads the existing job with `GetByIdempotencyKey` and answers `200` with it and `Idempotent-Replayed: true`.
- **Removed:** that branch (`if false && errors.Is(err, job.ErrDuplicateKey)`), so the error went to `storeError` like any other.
- **Caught by:** `TestIdempotentCreateUnderConcurrency` (20 concurrent HTTP requests with one key, real PostgreSQL) and three handler tests.
- **Observed:**
  ```text
  --- FAIL: TestIdempotentCreateUnderConcurrency (0.07s)
      idempotency_test.go:65: request 0: status 500, want 201 or 200
      ...                                    (requests 1-7 and 9-19 the same)
      idempotency_test.go:68: request 8 got job "2ec2e1f9711c1d6bcb4e9001699f920f", request 0 got ""
      idempotency_test.go:72: 1 created, 0 replayed; want 1 and 19
  (which request wins, and the job ID, change every run)
  --- FAIL: TestCreateWithTheSameKeyReturnsTheFirstJob (0.00s)
      idempotency_test.go:35: repeated create: status 500, body {"error":"internal error"}
  ```
- **Why:** the data stayed correct even when broken: one job, because the unique index on `(user_id, idempotency_key)` refused the other 19 `INSERT`s. That is the half that must live in the database: a "look the key up first, insert if missing" check in Go lets concurrent requests all see "missing". The other half is the answer. A client sends the same key again exactly because it doesn't know what happened; a `500` tells it "something broke, try again", and a client that keeps trying gets `500` forever while its job runs. The loser of the race has to get the winner's job, the same answer as if it had been first.

## 35. An event reaches only the job's owner

- **Safeguard:** `Hub.Deliver(userID, msg)` hands a message only to `h.streams[userID]`, the open streams of the user in the event's envelope. The envelope's `user_id` comes from the job row as committed (`RETURNING`), never from a client.
- **Removed:** the lookup by owner. `Deliver` looped over every stream of every user.
- **Caught by:** `TestHubDeliversOnlyToTheOwner`, `TestListenRoutesMessagesToTheOwner`, and `TestLiveEventsEndToEnd` (two users, real PostgreSQL and Redis, workers in the API).
- **Observed:**
  ```text
  --- FAIL: TestHubDeliversOnlyToTheOwner (0.00s)
      hub_test.go:60: bob received "alice's job", want nothing
  --- FAIL: TestListenRoutesMessagesToTheOwner (0.00s)
      listen_test.go:58: bob received "{\"type\":\"deleted\",\"id\":\"j1\"}", want nothing
  --- FAIL: TestLiveEventsEndToEnd (2.09s)
      live_test.go:166: bob received alice's job: {"type":"changed","job":{"id":"911484f2d69e2e6d276cb2ab9d3be8dd","title":"live e2e","status":"pending",...}}
  ```
- **Why:** case 21 again, on a new path. Every job query filters by owner in SQL, so the REST API can't leak another user's job. Live updates bypass those queries: one Redis channel carries every user's events to every API instance, and the hub is the only place that can tell them apart. Without the lookup, anyone with an account opens a stream and watches every other user's job titles and statuses as they happen. The e2e test is what makes this hold end to end: it checks the routing with two real users, not just the hub in isolation.

## 36. Every change to a job raises its version

- **Safeguard:** every `UPDATE jobs` sets `version = version + 1` in the same statement (migration 0009). Live-update clients keep an event only if its `version` is higher than their copy's.
- **Removed:** `version = version + 1` from `Claim`'s `UPDATE` (the move to `processing`).
- **Caught by:** `TestEveryChangeRaisesTheVersion` (the whole lifecycle, step by step), `TestEveryJobUpdateBumpsTheVersion` (reads the package's source: every `UPDATE jobs` must raise the version), `TestReadsReturnTheVersion`, and `TestLiveEventsEndToEnd`.
- **Observed:**
  ```text
  --- FAIL: TestEveryChangeRaisesTheVersion (0.44s)
      version_test.go:73: after Claim: version 3, want 4
  --- FAIL: TestEveryJobUpdateBumpsTheVersion (0.00s)
      version_test.go:112: work.go: an UPDATE of jobs doesn't raise the version:
          UPDATE jobs SET status = 'processing', attempts = attempts + 1, next_attempt_at = NULL, ...
  --- FAIL: TestLiveEventsEndToEnd (2.16s)
      live_test.go:157: alice's view went pending → queued → completed
  ```
- **Why:** the end-to-end line is what a user would see. The `processing` event carried the same version as the event before it, so the client took it for a late copy and dropped it: the job seemed to jump from waiting to done. (Which events arrive changes from run to run: another run read `pending → completed`. `processing` is the one that's always lost.) Nothing crashed and the database was right, but the screen lied. A version only orders events if every change raises it, which is why one test reads the source: it fails for a statement written next year, not just the eight that exist today. Timestamps can't replace it: `now()` is when a transaction started, so the transaction that commits second can carry the earlier time.

## 37. Job titles are shown as text, never as HTML

- **Safeguard:** the dashboard builds every element with `createElement` and puts server text in with `textContent`, so a job title is only ever characters. The page is also served with `script-src 'self'`, which refuses inline code.
- **Removed:** the title cell set with `innerHTML: job.title` instead of `textContent`.
- **Caught by:** `TestScriptNeverTurnsTextIntoHTML`, which reads `app.js` and fails on `innerHTML`, `outerHTML`, `insertAdjacentHTML`, `document.write`, `eval` and `new Function`.
- **Observed:**
  ```text
  --- FAIL: TestScriptNeverTurnsTextIntoHTML (0.00s)
      web_test.go:108: app.js uses innerHTML
  ```
  Then in a headless Chromium, with the broken page and a job titled `<img src=x onerror=alert(1)>`:
  ```text
  img elements in table: 1
  error: Failed to load resource: the server responded with a status of 404 (Not Found)
  error: Executing inline event handler violates the following Content Security Policy directive 'script-src 'self''. ... The action has been blocked.
  ```
- **Why:** a job title is user input, and here it became markup: the browser created a real `<img>` and requested `/x` from our server. The attacker's code in `onerror` didn't run only because the second layer, the Content-Security-Policy, refused it. With `'unsafe-inline'` in the policy, it would have run with full access to `sessionStorage` and the 24-hour token. One layer failed and the other held; neither should be the only one. Escaping is the fix, the policy is the seatbelt. The test runs without a browser, so `check.sh` catches the mistake before anyone deploys it.

## 38. Logged header values are capped

- **Safeguard:** the `client ip sources` diagnostic (`GOFLOW_LOG_IP_HEADERS=on`) logs each forwarding header through `truncate(..., maxLoggedHeader)`: at most 200 bytes and a `...(cut)` marker.
- **Removed:** the cap. Each header value was logged in full.
- **Caught by:** `TestIPSourcesAreCapped`, which sends a 100,000-character `X-Forwarded-For`.
- **Observed:**
  ```text
  --- FAIL: TestIPSourcesAreCapped (0.00s)
      middleware_test.go:188: logged 100000 characters, want at most 208 and a ...(cut) marker
  ```
- **Why:** these headers come from the client, and Go's server accepts up to 1 MB of them per request. Without the cap, every request could write a megabyte of attacker-chosen text into the logs: a cheap way to fill a free plan's log storage and bury the lines that matter. Anything logged from a request needs a size limit, the same reason the request log never records query strings or bodies.

## 39. Unknown emails count against the login limit too

- **Safeguard:** `login` counts the per-email limit right after normalizing the email and **before** looking the user up, so a registered email and one nobody owns run into the limit at the same attempt with the same answer.
- **Removed:** the per-email check moved after a successful `GetByEmail`, so only registered emails were counted.
- **Caught by:** `TestUnknownEmailsAreLimitedLikeRealOnes`.
- **Observed:**
  ```text
  --- FAIL: TestUnknownEmailsAreLimitedLikeRealOnes (0.02s)
      limits_test.go:100: 11th attempt: registered 429 {"error":"too many attempts; try again in 900 seconds"}
          , unknown 401 {"error":"invalid email or password"}
          ; must be identical
  ```
- **Why:** the limit became an oracle. Send 11 wrong passwords for any email: `429` means someone has an account with it, `401` means nobody does. Task 17 made every login failure look and take the same time precisely so attackers can't list accounts (case 20); a limit counted in the wrong place undid that in eleven requests. The moved line still "worked": real accounts were protected, and every test about guessing passwords passed. Only a test comparing the two answers side by side notices.

## 40. Requests are counted by route, not by path

- **Safeguard:** `recordMetrics` labels each request with `r.Pattern`, the route the mux matched (`GET /api/v1/jobs/{id}`), and puts every request that matched nothing under `unmatched`.
- **Removed:** the label taken from `r.URL.Path`, the path as the client sent it.
- **Caught by:** `TestRequestsAreCountedByRoutePattern`.
- **Observed:**
  ```text
  --- FAIL: TestRequestsAreCountedByRoutePattern (0.01s)
      metrics_test.go:58: metrics lack goflow_http_requests_total{code="200",route="GET /api/v1/jobs/{id}"} 3
      metrics_test.go:58: metrics lack goflow_http_requests_total{code="404",route="unmatched"} 50
      ...
      metrics_test.go:63: metrics contain a path or ID: "69efb80df3c1ef797e858680bf1fc655"
      metrics_test.go:63: metrics contain a path or ID: "/random/"
      metrics_test.go:63: metrics contain a path or ID: "does-not-exist"
  ```
- **Why:** every distinct label value is a separate time series, kept in memory until the process restarts. With paths as labels, each job ID is a new series, and so is every URL anyone makes up: 50 made-up paths in the test were 50 series. An attacker needs no account to send a million different paths, and the server would hold a million counters and histograms. A hosted collector would refuse them past its series limit (about 10,000 on Grafana Cloud's free plan). The route pattern has a fixed number of values: one per route in the code, plus `unmatched`. Every label must come from a small, fixed set the code decides, never from the request.

## 41. An unreadable queue is reported as unknown, not empty

- **Safeguard:** the queue collector asks Redis for the list sizes when `/metrics` is read, and if Redis doesn't answer within a second it sends no `goflow_queue_jobs` lines at all.
- **Removed:** the early return. On an error the collector reported 0 for every list.
- **Caught by:** `TestQueueSizesAreMissingWhenRedisFails`.
- **Observed:**
  ```text
  --- FAIL: TestQueueSizesAreMissingWhenRedisFails (0.00s)
      goflow_test.go:76: queue sizes reported while Redis was down:
          goflow_queue_jobs{list="delayed"} 0
          goflow_queue_jobs{list="pending"} 0
          goflow_queue_jobs{list="processing"} 0
  ```
- **Why:** zero is a claim, "nothing is waiting", and during a Redis outage it's exactly the wrong one. Jobs are still being created and saved to the outbox; nothing reaches Redis. A graph or an alert on queue depth would show a calm, empty queue at the moment the system is stuck. A missing value shows up as a gap, which is what a monitoring system needs to say "no data" and alert on that instead. The same rule as the dashboard's dot in Phase 10's requirement: never report a healthy-looking state you haven't measured.

## 42. Only a canary that came back proves delivery

- **Safeguard:** `events.Watch` publishes a canary every 2 seconds and ignores whether `PUBLISH` succeeded. Only `Listen` receiving the hub's own canary through the subscription counts as "delivery works".
- **Removed:** that rule. A successful `PUBLISH` counted as a canary that came back.
- **Caught by:** `TestAPublishThatNeverArrivesIsAPause` and `TestOnlyTheHubsOwnCanariesCount`. The e2e test `TestLiveDeliveryPausesWhileRedisIsAway` still passed: with Redis unreachable, `PUBLISH` fails too, so the broken rule happened to give the right answer there.
- **Observed:**
  ```text
  --- FAIL: TestAPublishThatNeverArrivesIsAPause (1.00s)
      health_test.go:155: no pause within 1s: PUBLISH succeeded but no canary came back, and delivery still counts as working
  --- FAIL: TestOnlyTheHubsOwnCanariesCount (1.00s)
      health_test.go:178: no pause within 1s: only another hub's canaries came back, and delivery still counts as working
  ```
- **Why:** `PUBLISH` succeeding means Redis took the message. It says nothing about whether this API's subscription is alive: a half-open subscriber connection, or a subscription that never came back after a reconnect, still lets every `PUBLISH` succeed while no event reaches the hub. That's exactly the failure the check exists for, the dot saying `live` over a list that has stopped moving. Measure at the end of the path, where the result is used, not at the step you happen to control. It's also why the fakes break each half of the path on its own: a test that only cuts everything at once (like the e2e one) can't tell the right rule from a wrong one that agrees with it in that one case.

## 43. The image's binaries are static

- **Safeguard:** `ENV CGO_ENABLED=0` in the Dockerfile's build stage.
- **Removed:** that line, so Go builds with its default. The `golang` image has a C compiler, so the default is CGO on.
- **Caught by:** `TestImageIsStaticAndRunsAsNonroot`, which reads the Dockerfile (check.sh can't run Docker). The image itself still builds without complaint.
- **Observed:**
  ```text
  --- FAIL: TestImageIsStaticAndRunsAsNonroot (0.00s)
      image_test.go:57: Dockerfile builds without CGO_ENABLED=0: the binary would need a C library the final image doesn't have
  ```
  The same build outside Docker, and both binaries started in an empty root directory, the way distroless `static` has no C library:
  ```text
  $ file api-cgo api-static
  api-cgo:    ELF 64-bit LSB executable, x86-64, dynamically linked, interpreter /lib64/ld-linux-x86-64.so.2
  api-static: ELF 64-bit LSB executable, x86-64, statically linked
  $ chroot emptyroot /api-cgo
  chroot: failed to run command '/api-cgo': No such file or directory
  $ chroot emptyroot /api-static
  level=ERROR msg="invalid configuration" err="GOFLOW_DATABASE_URL is required"
  ```
  In the real image, `docker run` stops with `exec /usr/local/bin/api: no such file or directory`.
- **Why:** with CGO on, Go's DNS resolver and user lookup call the system C library, so the binary is linked against it and names a loader, `/lib64/ld-linux-x86-64.so.2`, that the kernel must start first. distroless `static` has neither. The kernel reports the missing loader as "no such file or directory", about a file that is plainly there, which sends people looking in the wrong place. The static binary started and got as far as its own settings check. Nothing failed at build time: the mistake shows up only when the container starts, which is why a test reads the Dockerfile.

## 44. Settings never enter the image

- **Safeguard:** the Dockerfile declares no `ARG` and sets no `GOFLOW_` variable with `ENV`. Settings reach the container when it starts.
- **Removed:** that rule, the convenient way. Two lines in the final stage turn a build argument into an image setting:
  ```dockerfile
  ARG GOFLOW_METRICS_TOKEN
  ENV GOFLOW_METRICS_TOKEN=$GOFLOW_METRICS_TOKEN
  ```
- **Caught by:** `TestImageHoldsNoSettingsOrSecrets`, which reads the Dockerfile. The image builds and runs fine either way.
- **Observed:**
  ```text
  --- FAIL: TestImageHoldsNoSettingsOrSecrets (0.00s)
      image_test.go:90: Dockerfile declares "ARG GOFLOW_METRICS_TOKEN": build arguments end up in the image's history
      image_test.go:93: Dockerfile sets "ENV GOFLOW_METRICS_TOKEN=$GOFLOW_METRICS_TOKEN": settings come from the environment at run time
  ```
  Built locally with a made-up value (`--build-arg GOFLOW_METRICS_TOKEN=a-fake-token-for-case-44`), `docker inspect --format '{{json .Config.Env}}'` lists it among the image's settings, where anyone who can pull the image can read it.
- **Why:** on Render this needs no `--build-arg`: Render passes every environment variable of the service to the build as a build argument. With these two lines, each deploy would copy the real metrics token into the image's configuration and history. Nothing would look wrong: the token would even work. An image is a file that gets copied, cached and shared; a secret in it is as good as published to everyone who ever gets a copy. Settings belong to the running container, not to the image.

## 45. Prometheus sends the token the API expects

- **Safeguard:** the development metrics token appears twice, in `compose.yaml` (the API's `GOFLOW_METRICS_TOKEN`) and in `monitoring/prometheus.yml` (what Prometheus sends), and a test keeps the two equal.
- **Removed:** the agreement. The token was changed in `prometheus.yml` only, as happens when one of two copies is edited.
- **Caught by:** `TestPrometheusUsesTheAPIsMetricsToken`, which reads both files.
- **Observed:**
  ```text
  --- FAIL: TestPrometheusUsesTheAPIsMetricsToken (0.00s)
      monitoring_test.go:61: Prometheus sends "local-development-metrics-token-renamed-here", the API expects "local-development-metrics-token-not-a-secret": every scrape would get 401
  ```
  With the stack running, Prometheus's target page (`http://localhost:9090/targets`) shows `goflow-api` as down with a 401 error, and every Grafana panel says "No data".
- **Why:** nothing else complains. Prometheus keeps running, Grafana keeps drawing empty graphs, and the API answers the wrong token with the same `401` it gives anyone. Empty graphs look like a quiet system. Two copies of one value drift apart; when a value must be in two files, a test that compares them is the cheapest guard.

## Experiment: the naive dual write loses a job

Not a removed safeguard but the design of Task 22, run live on 2 November 2026 to see the failure Task 23 fixes. `POST /api/v1/jobs` saved the job in PostgreSQL, then pushed its ID to Redis, with nothing tying the two together.

- **Steps:** create a job, stop Redis (`docker stop goflow-redis`), create another, list jobs, start Redis, read the list.
- **Observed:**
  ```text
  2676a8c2fc57deea9b627d89b7f76ed7                      # "queued fine", 201 in 5 ms
  1                                                     # LLEN goflow:jobs:pending
  error: create: internal error (HTTP 500)              # "lost in between", Redis stopped
  7a7a19b4304aae6b6f01468fa8d46b4a  pending  2026-11-02 22:19:03  lost in between
  2676a8c2fc57deea9b627d89b7f76ed7                      # LRANGE after Redis restarted
  ```
  Server log for the failed request, after 1.74 s of retries:
  ```text
  level=ERROR msg="internal error" err="queue: enqueue 7a7a19b4304aae6b6f01468fa8d46b4a: dial tcp 127.0.0.1:6379: connect: connection refused"
  ```
- **Why it matters:** the client was told the job failed, but it exists. A retry would create a second copy. The saved one is never queued, and nothing in the system notices: it stays `pending` forever, like the two jobs created before the queue existed. Two writes to two systems can't succeed or fail together. Task 23 writes the job and an outbox row in one PostgreSQL transaction and lets a relay deliver the ID to Redis later.
- **Also seen:** go-redis logged its retries with its own logger (`redis: 2026/11/02 22:19:03 pool.go:770: ... failed to dial after 5 attempts`), outside GoFlow's structured logs and without a request ID.

## Experiment: the outbox survives a Redis outage

Task 23, run live on 2 November 2026 on the database left behind by the dual-write experiment.

- **On start**, migration 6 gave the four jobs still `pending` an outbox row, and the relay queued them at once:
  ```text
  msg="migrations applied" versions=[6]
  msg="relay: jobs queued" count=4
  ```
  Redis then held `2676a8c2fc57deea9b627d89b7f76ed7` **twice**. Task 22 had pushed it but never marked it `queued`, so the backfill queued it again. That is at-least-once delivery, seen live: workers (Phase 6) must handle the same job twice.
- **With Redis stopped**, `goflow job create survives an outage` succeeded (`201` in 2.7 ms), the job was `pending`, and its row waited in `job_outbox`:
  ```text
   id |              job_id              |          created_at
    5 | 88cb882b2242e5a7a4a9de53f9cfd358 | 2026-11-02 21:39:00.73624+00
  ```
- **When Redis came back**, the relay queued it: `msg="relay: jobs queued" count=1`, 0.3 s after the create. The relay's push had been retrying inside go-redis (about 1.7 s of dial retries) when Redis started, so the retry succeeded and no "will retry" warning was logged. A longer outage would leave the row for the next 30-second tick.
- **Compared with the dual write:** same outage, but the request succeeded and the job reached the queue by itself.

## Experiment: a worker stopped mid-job

Task 24, run live on 3 November 2026 with the API and one worker.

- **The backlog:** the worker completed the six jobs left from the earlier experiments, 5 s each, and skipped the duplicate ID: `worker: skipping, already finished or deleted job_id=2676a8c2fc57deea9b627d89b7f76ed7`. At-least-once delivery, but the job ran once.
- **The crash:** a job was created, and the worker was sent SIGINT 2 s into its 5 s of work:
  ```text
  00:03:23.030 worker: stopped mid-job; it will run again after restart job_id=1627a0c861d907904df6884e9f6d772e
  --- processing list:
  1627a0c861d907904df6884e9f6d772e
  1627a0c861d907904df6884e9f6d772e  processing  2026-11-03 00:03:20  crash for real
  ```
- **The restart:**
  ```text
  00:03:26.818 worker: unfinished jobs put back in the queue count=1
  00:03:31.827 worker: job completed job_id=1627a0c861d907904df6884e9f6d772e duration=5.006s
  1627a0c861d907904df6884e9f6d772e  completed  2026-11-03 00:03:20  crash for real
  ```
- **Timing note:** two earlier attempts pressed Ctrl+C by hand after the job had already completed (5 s and 30 s too late), and one ran with the API stopped. The shell timed it reliably: `./bin/worker & W=$!`, then `sleep 2; kill -INT $W`.
---

## What tests did not catch

- **`Lock` instead of `RLock` in `Get`:** every test passed. The code is correct but reads queue one at a time. Correctness tests can't measure throughput. That needs a benchmark (planned for Phase 13).
- **A cheap bcrypt cost:** setting `DefaultCost` to 4 passes every test, because tests use cost 4 on purpose. Production hashes would be about 240 times cheaper to crack. Fixed in Task 17: the cost comes from `GOFLOW_BCRYPT_COST`, the server refuses anything below 10, and `TestLoadConfigDefaults` expects a literal 12 (case 19).
- **Timing while the bcrypt cost is raised:** with `GOFLOW_BCRYPT_COST` raised from 12 to 13, two unknown emails took 0.91 s and 0.95 s, while a registered user whose hash was still at cost 12 took 0.47 s. Until every user has logged in once at the new cost, response time can still tell old accounts apart from unknown emails. Tests run at a single cost, so they cannot see this. Narrowed in Task 19: a successful login now re-hashes to the configured cost in both directions, so the gap lasts only until each user next logs in.
- **A successful `PUBLISH` counted as delivery (case 42):** the end-to-end test `TestLiveDeliveryPausesWhileRedisIsAway` passed with the broken rule. It cuts Redis completely, so the canary's `PUBLISH` fails too, and the wrong rule pauses at the same moment as the right one. Only the unit tests, which let `PUBLISH` succeed while the subscription delivers nothing, tell them apart. A test proves a rule only where the right and the wrong rule would give different answers.

## Environment incidents

- **Tests migrated the development database.** `schema_migrations.applied_at` showed migration 1 applied 13 seconds before the API first started, matching the time `./scripts/check.sh` ran. The end-to-end test was using the development database URL. Fixed by giving tests their own database (`goflow_test`).
- **DNS timeouts on a phone hotspot stopped the API from starting.** Against Neon from a laptop on a phone's mobile hotspot, startup failed with `hostname resolving error: ... i/o timeout`. `getent ahosts` for the same host took 5.06 s once and 0.01 s later: most likely the hotspot dropped the first DNS query, and the API's 5-second startup ping ran out first. Worked around with `ipv4.dns-options "single-request-reopen,timeout:2,attempts:3"` on that connection only (Go reads these options from `/etc/resolv.conf`). The same hotspot makes the first request after a few idle minutes take 2–5 s ([DEPLOYMENT.md](DEPLOYMENT.md#measured-2-november-2026)).

- **A deploy failed because the API started before its queue.** On Render, one Blueprint sync created the Key Value instance and redeployed the API with its URL. The new instance logged `starting ... queue_enabled=true workers=2` twice and exited; the old one kept serving. A manual redeploy minutes later worked. The API exited when its startup Redis ping failed, even though everything that uses Redis retries on its own. Fixed in Task 28: an unreachable Redis is a warning at startup, not a fatal error ([DEPLOYMENT.md](DEPLOYMENT.md#queue-and-workers-task-27)).
- **The IP diagnostic stayed on in production for about 18 hours.** Task 37 turned `GOFLOW_LOG_IP_HEADERS` on in Render's dashboard on 4 November (22:53 UTC) for one experiment; its last step, "turn it off", wasn't done. Every request then logged the visitor's address and forwarding headers until 5 November, 16:46 UTC, when the startup warning (`GOFLOW_LOG_IP_HEADERS is on ... turn it off when the check is done`) was spotted while reading Task 45's logs. The warning was in every startup log the whole time; nobody reads startup logs unless something is wrong. Settings typed into the dashboard aren't in `render.yaml`, so no merge removes them. Lesson: a diagnostic that records personal data needs an end built in (a deadline after which it switches itself off), not only a warning; and a checklist item to remove it, checked before the task is called done.

## Not yet reproduced

These safeguards have tests, but the failure hasn't been run and recorded yet:

- `store.Save` in the CLI's `create` command (expected: `TestCreateThenGet` fails, because each command is a separate process).
