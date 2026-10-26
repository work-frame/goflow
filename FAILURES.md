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

---

## What tests did not catch

- **`Lock` instead of `RLock` in `Get`:** every test passed. The code is correct but reads queue one at a time. Correctness tests can't measure throughput. That needs a benchmark (planned for Phase 13).

## Environment incidents

- **Tests migrated the development database.** `schema_migrations.applied_at` showed migration 1 applied 13 seconds before the API first started, matching the time `./scripts/check.sh` ran. The end-to-end test was using the development database URL. Fixed by giving tests their own database (`goflow_test`).

## Not yet reproduced

These safeguards have tests, but the failure hasn't been run and recorded yet:

- `r.WithContext(ctx)` in `withRequestID` (expected: `TestLogRequestsWritesOneLinePerRequest` fails, while the `X-Request-ID` response header still works).
- `store.Save` in the CLI's `create` command (expected: `TestCreateThenGet` fails, because each command is a separate process).


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

## Not yet reproduced

These safeguards have tests, but the failure hasn't been run and recorded yet:

- `store.Save` in the CLI's `create` command (expected: `TestCreateThenGet` fails, because each command is a separate process).


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