# Development

## Prerequisites

- Go 1.27 or newer
- Docker with Compose (PostgreSQL, Valkey, and the API's image)
- `goimports`: `go install golang.org/x/tools/cmd/goimports@latest`
- `jq`, and the GitHub CLI `gh` (`gh auth login` once)
- `$(go env GOPATH)/bin` on your `PATH`

### Editor (VS Code or Code - OSS)

Install the Go extension (`golang.go`), **trust the workspace** (Restricted Mode disables extensions), and add to your user settings:

```json
"[go]": {
    "editor.defaultFormatter": "golang.go",
    "editor.formatOnSave": true,
    "editor.codeActionsOnSave": { "source.organizeImports": "explicit" }
}
```

Edits made with `sed` or heredocs in the terminal bypass the editor. Run `goimports -w <file>` after them.

### The development stack

PostgreSQL 17 and [Valkey](https://valkey.io) 8 (the open-source fork of Redis, and what Render's Key Value runs) come from [compose.yaml](compose.yaml):

```bash
docker compose up -d --wait postgres valkey
```

`--wait` returns once both answer their health checks. Run it at the start of every session (it replaces `docker start goflow-postgres`); it does nothing when they're already up. Ports are published on `127.0.0.1` only: Docker's port rules bypass most host firewalls. The first start on an empty volume also creates `goflow_test` ([scripts/postgres-init.sql](scripts/postgres-init.sql)). Development uses Valkey database 0 and tests use database 1, like `goflow` and `goflow_test` in PostgreSQL.

**Moving from the containers started by hand (before Phase 11).** `goflow-postgres` and `goflow-redis` hold ports 5432 and 6379, and compose uses their volumes (`goflow-pgdata`, `goflow-redisdata`), so your data comes along. Stop them once, then start the stack:

```bash
docker stop goflow-postgres goflow-redis
docker compose up -d --wait postgres valkey
```

Compose then warns that the volumes "already exist but were not created by Docker Compose": expected, and the data is kept. The stopped containers do nothing; don't start them again while the stack runs. `docker rm goflow-postgres goflow-redis` deletes the two containers when you're sure, never the volumes.

Everything at once: the API built from the [Dockerfile](Dockerfile) with two workers of its own and registration open, and three worker containers from the same image (`goflow-worker-1` to `-3`, two workers each):

```bash
docker compose up -d --build
docker compose logs -f api worker   # Ctrl+C stops following, not the stack
```

`docker compose up -d --scale worker=5` runs five worker containers instead, until the next plain `up`. All of them share one PostgreSQL and one Valkey, and each job is held under a lease, so a job runs on one worker at a time. A `bin/worker` started on the host joins them too: stop the containers first (`docker compose stop worker`) if you want it to get the jobs.

Docker may warn that the legacy builder is deprecated: install the buildx plugin (`sudo pacman -S docker-buildx` on Arch) to build with BuildKit. The Dockerfile works with both.

The API is then at `http://localhost:8080`, like one started with `go build`. Run one or the other: both want port 8080. `docker compose stop api` frees it; `docker compose down` stops the whole stack and keeps the volumes. Never add `-v` to `down`: it deletes the volumes, your development database with them.

### Watching it: Prometheus and Grafana

Opt-in, with the profile `monitoring`:

```bash
docker compose --profile monitoring up -d --build
```

| URL | What |
|---|---|
| `http://localhost:9090/targets` | Prometheus: `goflow-api` should be **UP**, read every 15 s |
| `http://localhost:3000` | Grafana: Dashboards → GoFlow → **GoFlow overview**. You can look without logging in; the admin login is `admin` / `goflow` (development only) |

Compose gives the API a development metrics token, and [monitoring/prometheus.yml](monitoring/prometheus.yml) sends the same one (`TestPrometheusUsesTheAPIsMetricsToken` keeps them equal). The dashboard is a file, [monitoring/grafana/dashboards/goflow.json](monitoring/grafana/dashboards/goflow.json): change it there, not in the browser, where changes aren't saved. Create a few jobs (`flaky:1`, `flaky:3`, `sleep:5s`) in the dashboard at `http://localhost:8080` and the graphs fill in within a minute. `docker compose stop prometheus grafana` stops just the two; Prometheus keeps its numbers in the `promdata` volume.

### Crash test: kill a worker mid-job

What happens when a worker process dies without warning, the way it does when the machine loses power or the kernel kills it for memory. `docker kill` sends SIGKILL: the process stops at once, with no drain, no last write and no message to anyone. It stops that one container and deletes nothing; `docker compose up -d` starts it again.

The rule under test: the job comes back only after PostgreSQL says its lease ran out, on another worker, as attempt 2. Every other job keeps running and finishes on attempt 1.

1. Start the stack and log in to the local API with your development account (`goflow login` saves one API at a time):

   ```bash
   docker compose up -d --build
   ./bin/goflow login
   ```

2. Create eight 12-second jobs, one for each worker slot (three containers with two each, plus the API's two). 12 s stays under the 15-second attempt limit:

   ```bash
   for i in 1 2 3 4 5 6 7 8; do ./bin/goflow job create sleep:12s; done
   docker compose logs worker | grep 'job claimed'
   ```

   Each line starts with the container that claimed the job, for example `worker-2  | ... msg="worker: job claimed" ... job_id=e448d133... attempt=1`. Pick a container that has at least one.

3. Kill it, within 12 s of step 2 (before its jobs finish), then watch the jobs. Replace `2` with the container you picked:

   ```bash
   docker kill goflow-worker-2
   for i in $(seq 25); do docker compose exec -T postgres psql -U goflow -tAF' ' -c "SELECT to_char(now(),'HH24:MI:SS'), left(id,8), status, attempts, to_char(lease_expires_at,'HH24:MI:SS.MS'), round(extract(epoch FROM lease_expires_at - now())) FROM jobs WHERE title = 'sleep:12s' AND created_at > now() - interval '5 minutes' AND status <> 'completed' ORDER BY id"; echo; sleep 2; done
   ```

   The columns are PostgreSQL's time, the job, its status, attempts, lease deadline and seconds left. The jobs of the other workers finish and drop out of the list. The killed worker's jobs stay `processing`, attempt 1, with a deadline that no longer moves (nobody renews it) and seconds left counting down to 0 and below. Only then, within the reaper's 5-second check, they turn into attempt 2 with a fresh 30-second deadline, and finish 12 s later.

4. Check who took them back, and when:

   ```bash
   docker compose logs api worker | grep -E 'lease ran out|attempt=2'
   docker compose exec -T postgres psql -U goflow -c "SELECT attempts, count(*) FROM jobs WHERE title = 'sleep:12s' AND created_at > now() - interval '10 minutes' GROUP BY attempts ORDER BY attempts"
   ```

   `lease reaper: lease ran out; job sent to run again` must be later than the deadline step 3 showed, and the `attempt=2` claims come from a container other than the one you killed. The count is the killed container's jobs on attempt 2 and every other job on attempt 1. A job on attempt 2 from any other container, or a reaper line before the deadline, is a bug.

5. Start the killed container again: `docker compose up -d`. Its startup tidies the processing list and leaves the other workers' jobs alone.

Expect the wait to be 20 to 30 s after the kill. A lease lasts 30 s from its last renewal (or the claim), and renewals come every 10 s, so the last one was at most 10 s before the kill. However short or long it is, the job waits for its own deadline.

### Environment

```bash
export GOFLOW_DATABASE_URL='postgres://goflow:goflow@127.0.0.1:5432/goflow?sslmode=disable'
export GOFLOW_TEST_DATABASE_URL='postgres://goflow:goflow@127.0.0.1:5432/goflow_test?sslmode=disable'
export GOFLOW_REDIS_URL='redis://127.0.0.1:6379/0'
export GOFLOW_TEST_REDIS_URL='redis://127.0.0.1:6379/1'
```

| Variable | Used by | Default |
|---|---|---|
| `GOFLOW_DATABASE_URL` | API (required); a remote host needs `sslmode=verify-full` or `require` | none |
| `GOFLOW_ADDR` | API | `:8080` |
| `GOFLOW_BCRYPT_COST` | API; 10 to 14, refused otherwise | `12` |
| `GOFLOW_REGISTRATION` | API; `open` or `closed` | `closed` |
| `PORT` | API, when `GOFLOW_ADDR` is unset (hosting platforms) | none |
| `GOFLOW_REDIS_URL` | API; optional, remote hosts need `rediss://` | none (queue off) |
| `GOFLOW_TEST_REDIS_URL` | tests; Redis tests skip without it, and `check.sh` refuses to run | none |
| `GOFLOW_WORKERS` | worker: jobs that run at once, 1 to 32 (default 4). API: workers it runs itself, 0 to 32 (default 0; needs `GOFLOW_REDIS_URL`) | see left |
| `GOFLOW_TEST_DATABASE_URL` | tests; database tests skip without it, and `check.sh` refuses to run | none |
| `GOFLOW_QUICK_CHECK` | `check.sh`; `1` runs without the two test URLs and says it was a quick check | none |
| `GOFLOW_API_URL` | CLI | `http://localhost:8080` |
| `GOFLOW_TOKEN` | CLI; overrides the saved login from `goflow login` (scripts, CI) | none |

Tests must never point at the development database.

Migration 4 (job owners) deletes every job that existed before accounts did, because no user can own them. In a fresh database it deletes nothing.

## Workflow

```bash
git checkout main && git pull
git checkout -b feat/short-description
# change code and tests
./scripts/check.sh && git add . && git status && git commit -m "feat(scope): what changed"
git push -u origin feat/short-description
gh pr create --base main --title "feat(scope): what changed" --body "Why, and what to look at."
gh pr merge --squash --subject "feat(scope): what changed" --delete-branch
git checkout main && git pull && ./scripts/check.sh && git log --oneline -3 && git fetch --prune
```

Branch prefixes: `feat/`, `fix/`, `test/`, `docs/`, `chore/`. Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/).

## Checks

`./scripts/check.sh` runs `gofmt`, `go vet` and `go test -race -count=1 ./...`, stopping at the first failure and naming it.

It refuses to start unless `GOFLOW_TEST_DATABASE_URL` and `GOFLOW_TEST_REDIS_URL` are both set. A test that needs PostgreSQL or Redis skips itself without its URL, and `go test` prints `ok` for a package whose tests skipped, so a check without them looks like a pass and tests far less. That happened: the Redis tests skipped on the development laptop from Phase 5 to Task 48 ([FAILURES.md](FAILURES.md#environment-incidents)). For a quick check without the services, ask for it by name:

```bash
GOFLOW_QUICK_CHECK=1 ./scripts/check.sh   # ends with QUICK CHECK PASSED, never ALL CHECKS PASSED
```

Only `ALL CHECKS PASSED` counts before a commit.

### CI

[.github/workflows/ci.yml](.github/workflows/ci.yml) runs on every pull request and every push to `main`, with two jobs:

- **check:** `./scripts/check.sh` on a fresh Ubuntu runner, with PostgreSQL 17 and Valkey 8 as service containers (the images `compose.yaml` runs) and the latest Go of the minor version the Dockerfile builds with. The database password exists only inside the run.
- **govulncheck:** [govulncheck](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck) at a pinned version. It reports known vulnerabilities in GoFlow's dependencies and in the Go standard library, and fails only for code GoFlow actually calls.

`gh pr checks --watch` follows a pull request's checks from the terminal. A green CI run says the code passed the check on a clean machine. It says nothing about the docs: keeping DEVELOPMENT.md, ARCHITECTURE.md and the README true stays part of reviewing each pull request.

The workflow's rules are tested in `cmd/api/ci_test.go` (FAILURES.md, case 55): every action pinned to a full commit SHA with its tag in a comment, `permissions: contents: read`, `persist-credentials: false` on every checkout, no secrets, no `@latest`. To update an action, look up the tag's commit and read the release notes before pasting the SHA:

```bash
git ls-remote https://github.com/actions/setup-go 'refs/tags/v7*'
```

## Tests

```bash
go test -count=1 ./...                               # everything
go test -count=1 -v -run TestJobStore ./internal/database/
go test -count=5 -race ./...                         # shake out intermittent races
go test -count=1 -timeout 10s ./internal/cli/        # fail fast if something loops
```

See the Testing section of [ARCHITECTURE.md](ARCHITECTURE.md) for what each package's tests cover.

### Load testing

`cmd/loadgen` creates jobs through the real API on this machine, follows each one to `completed`, and reports jobs completed per second and the time from create to completion (p50, p95, p99). It refuses any API that isn't `localhost` or a loopback address, before sending anything: never point load at the hosted API. It reads live events for speed and asks the API about jobs no event has finished yet, because events can be missed. Jobs that end dead or don't finish within `-wait` are listed by ID, and the command then exits 1. It deletes the jobs it created after the report (`-keep` keeps them).

```bash
docker compose up -d --wait
go build -o bin/goflow ./cmd/goflow && ./bin/goflow login   # the local development account
./scripts/loadtest.sh                                        # 1, 3 and 6 worker containers, 500 jobs, 3 runs each
GOFLOW_LOADTEST_JOBS=100 GOFLOW_LOADTEST_RUNS=1 ./scripts/loadtest.sh   # a quick look
```

It starts with one job on the stack as it is, so a missing login or a stack that isn't up stops it before anything is recreated. `loadtest.sh` then recreates the `api` and `worker` containers for each step with the API's own workers off (`GOFLOW_API_WORKERS=0`), so only the worker containers run jobs, and counts the workers that actually started from their logs. PostgreSQL and Valkey keep running. At the end it puts the stack back: three worker containers, API workers on.

Measured on the development laptop (11 November 2026; 8 threads, everything in Docker on the same machine, 500 `sleep:0s` jobs, 20 creates in flight, API workers off):

| Worker containers | Workers | Jobs/s (3 runs) | Mean | p50 create→done | p95 create→done | Seen by polls |
|---|---|---|---|---|---|---|
| 1 | 2 | 290, 398, 334 | 341 | 448–587 ms | 0.88–1.21 s | 0, 7, 1 |
| 3 | 6 | 751, 840, 832 | 807 | 204–211 ms | 222–225 ms | 0 |
| 6 | 12 | 496, 334, 636 | 489 | 94–190 ms | 161–283 ms | 0 |

Every job completed in every run. Two workers are the limit at the first step: jobs wait for one, so the time to completion is long and the rate low. Six workers give 2.4 times the rate, with steady times. Twelve gave a *lower* rate than six in these runs, and varied from run to run, although each job, once claimed, finished sooner (lower p50). These runs didn't record how fast the jobs were created, and 500 jobs last well under a second, so they can't say why. The rerun below, with longer runs and the create rate, did not repeat it.

Rerun on the same laptop (14 November 2026, commit `bde45f8` (merged as `5ac0740`)), 2000 jobs per run so each lasts 2 to 3 seconds, with the create rate and one CPU reading per run:

```bash
GOFLOW_LOADTEST_CONTAINERS="3 6" GOFLOW_LOADTEST_JOBS=2000 GOFLOW_LOADTEST_STATS=1 ./scripts/loadtest.sh
```

| Worker containers | Workers | Creates/s (3 runs) | Completions/s (3 runs) | Mean completions/s | Completions ÷ creates | p50 create→done | p95 create→done |
|---|---|---|---|---|---|---|---|
| 3 | 6 | 900, 1173, 1120 | 647, 822, 681 | 717 | 0.61–0.72 | 720–861 ms | 0.78–1.09 s |
| 6 | 12 | 853, 942, 948 | 836, 926, 933 | 898 | 0.98 | 69–103 ms | 91–167 ms |

All 12,000 jobs completed, none dead, none seen only by polling.

- **Six workers are the limit at 2000 jobs.** They finish 28 to 39% slower than the API accepts jobs, so a queue builds and jobs wait: p50 is 0.7 to 0.9 s.
- **Twelve workers keep up.** Completions follow creates within 2%, and a job waits about 70 to 100 ms. The completion rate here is the create rate, so twelve workers' own limit is above about 930 jobs per second and wasn't reached: a faster generator or a second API would be needed to find it.
- **The 500-job result above didn't repeat.** Twelve workers were 25% faster than six here. One possible reason, not tested: 500-job runs end in under a second, where start-up and timing noise weigh more.
- **CPU, one reading per run, as clues only:** the API used 118–137% (about 1.2 to 1.4 of the 8 threads) in all six readings, PostgreSQL 137–231%, Valkey 21–40%, each worker container 6–63%. The containers added up to roughly 3.2 to 5.3 threads, plus the generator, which runs outside Docker and wasn't measured. Creates were somewhat slower with twelve workers (mean 914 against 1064 per second); twelve workers sharing PostgreSQL and the CPU with the API is a likely reason, not a measured one.
- **Against the benchmark:** `ClaimAndComplete` takes about 4.2 ms on this laptop, about 240 jobs per second for one worker on an idle database. Six workers would reach about 1400 if nothing else ran; they reached 717 while the API was writing new jobs to the same PostgreSQL. The difference hasn't been broken down.

Two more settings: `GOFLOW_LOADTEST_CONTAINERS` picks the steps (default `1 3 6`), and `GOFLOW_LOADTEST_STATS=1` takes one `docker stats` CPU reading of every container half a second into each run. One reading is a clue, not a measurement; give the runs enough jobs to last a few seconds. The report's `creates:` line says how fast the API accepted the jobs: if that is close to the completion rate, creating is the limit, not the workers.

### Benchmarks

`scripts/bench.sh` runs the benchmarks 10 times each and summarises them with [benchstat](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat), which reports each mean with its spread and only calls a difference real when it's bigger than the noise. They're not in `check.sh` (minutes, and machine-dependent), but `check.sh` compiles them.

| Benchmark | Measures | Needs |
|---|---|---|
| `BenchmarkClaimAndComplete` | one job's `Claim` and `Complete` together, on a copy of the database: the pair, round trips included, not either one alone | PostgreSQL |
| `BenchmarkRelayPass` | one relay pass over exactly 100 outbox rows, with a Redis that accepts at once: the database's side of delivery | PostgreSQL |
| `BenchmarkMemoryStoreGet` | `Lock` against `RLock` with 1 and 8 parallel readers: the Phase 2 question. `MemoryStore` is test-only now, so this says nothing about production speed | nothing |

Every database benchmark gets a fresh copy of the template per run, and fails if an operation is refused: a benchmark never reports a speed for writes that didn't happen (FAILURES.md, case 54).

benchstat installs as a Go tool in `~/go/bin`, outside GoFlow's `go.mod`, at a pinned version. golang.org/x/perf has no release tags, so the version is a Go pseudo-version: look up the current one, install exactly that, and record it here.

```bash
go list -m golang.org/x/perf@latest                        # prints golang.org/x/perf v0.0.0-<date>-<commit>
go install golang.org/x/perf/cmd/benchstat@v0.0.0-<date>-<commit>
./scripts/bench.sh
```

Each run's header records the commit, Go version, OS, CPU, PostgreSQL version, benchstat version and the exact command.

Measured on the development laptop (14 November 2026): commit `bde45f8` (merged as `5ac0740`), go1.27.0 (X:nodwarf5), Linux 6.12.108 (Manjaro), Intel Core i5-8265U (4 cores, 8 threads), PostgreSQL 17.11 in Docker, benchstat `golang.org/x/perf v0.0.0-20261009192801-be2c69fb417e`, `./scripts/bench.sh` (10 runs each):

| Benchmark | Time per operation | Memory, allocations |
|---|---|---|
| `ClaimAndComplete` | 4.18 ms ± 14% | 5.0 KiB, 108 |
| `RelayPass` (100 rows) | 19.97 ms ± 7% | 113.5 KiB, 2109 |
| `MemoryStoreGet`, 1 reader | `RLock` 224.7 ns ± 1%, `Lock` 244.8 ns ± 1%: `Lock` 9% slower (p = 0.000) | |
| `MemoryStoreGet`, 8 readers | `RLock` 152.7 ns ± 3%, `Lock` 657.2 ns ± 46%: `Lock` 4.3 times slower (p = 0.000) | |

- `ClaimAndComplete` is the pair, round trips included: about 240 jobs per second for one worker with the database otherwise idle. It doesn't say how the time splits between `Claim` and `Complete`.
- `RelayPass` moves 100 jobs in 20 ms, about 5000 per second: far above any load test so far, so the relay isn't the limit.
- In the development sandbox, a different machine with PostgreSQL outside Docker, `ClaimAndComplete` took about 0.9 ms. Slower commits through Docker on the laptop are a likely reason for the gap, not a measured one; the hardware differs too.
- The Phase 2 question, answered: with one reader `RLock` saves a little; with eight, readers overlap under `RLock` and queue under `Lock`. `MemoryStore` is used by tests only, so this changes nothing in production.

### Finding slow tests

`go test -json` reports every test's time. The slowest ten in a package, top-level tests only:

```bash
go test -race -count=1 -json ./cmd/api/ | jq -r 'select(.Action == "pass" and .Test != null and (.Test | contains("/") | not)) | "\(.Elapsed)\t\(.Test)"' | sort -rn | head
```

Run it three or four times: a test that is slow only sometimes is a race or a timeout, not slow code. A time close to a round number (2 s, 5 s) usually means something is waiting for a timer to run out. To see what, build the test binary (`go test -c -race -o /tmp/api.test ./cmd/api`), run the one test many times (`-test.run '^TestName$' -test.count=60 -test.v`), and send the process `SIGQUIT` during a slow run: Go prints every goroutine and where it waits. That is how case 48 was found.

In `cmd/api` tests, get HTTP clients from `testClient(t, timeout)` after `startServer`, never `&http.Client{}` or `http.DefaultClient`: see case 48.

In `cmd/api`, `testDatabaseURL(t)` is the test's own database, copied from a template and dropped at the end; every call in one test returns the same copy. Build server configs with `testConfig`, which names the test's Redis queue keys and events channel after that copy. A test that needs two separate deployments side by side takes a second copy with `newTestDatabaseURL(t)`. Never use production's key names or `goflow:events` in a test (case 51).

Every test in `cmd/api` starts with `parallelTest(t)`, or `sequentialTest(t)` if it measures time or depends on when something happens; then add it to `timingTests` in `parallel_test.go` with the reason. `TestExactlyTheTimingTestsRunAlone` fails if a test starts with neither, or if the list and the marks disagree (case 52).

In `internal/database`, a test that gets its own copy (`newMigratedTestDB`, `newTestDB`, or a fixture built on them) starts with `t.Parallel()`. Its copy has 3 connections. A test that runs more callers at once than that calls `withConns(db, n)` and stays sequential (no `t.Parallel()`), so a few of them can't add up to PostgreSQL's limit of 100. A test that waits for a connection without asking fails with "waited N times for one of its 3 database connections" (case 50).

### Breaking things on purpose

Commit first. Then make the change, run the test, read the output, and restore with `git checkout -- <file>`. If the result is new, add it to [FAILURES.md](FAILURES.md).

## Running locally

```bash
go build -o bin/api ./cmd/api && GOFLOW_REGISTRATION=open ./bin/api   # terminal 1
```

Registration is closed by default. Open it locally, create your development account once with the `register` example in the [README](README.md#examples), then log in from terminal 2:

```bash
go build -o bin/goflow ./cmd/goflow
./bin/goflow login
id=$(./bin/goflow job create send welcome email)
./bin/goflow job get "$id"
./bin/goflow job list
```

In a third terminal, run the worker. It needs the same database and Redis as the API, and the API must have started once (it runs the migrations):

```bash
go build -o bin/worker ./cmd/worker && GOFLOW_REDIS_URL='redis://127.0.0.1:6379/0' ./bin/worker
```

`GOFLOW_WORKERS` sets how many jobs run at once (default 4, up to 32). Each job takes 5 seconds of placeholder work and ends `completed`. `Ctrl+C` stops taking new jobs and gives running ones 20 seconds to finish; a job still running after that records nothing, and once its 30-second lease runs out the lease reaper (in the API and in every worker process) sends it to run again.

Run as many worker processes as you like, next to an API that runs workers too: each job is held under a lease, and a process's start never touches jobs another one is running.

`goflow login` saves one API at a time. Logging in locally replaces a saved login to the hosted API, and the other way round.

Look at the data directly:

```bash
docker compose exec postgres psql -U goflow -c 'SELECT id, title, status FROM jobs'
docker compose exec postgres psql -U goflow -c 'SELECT * FROM job_outbox'          # jobs not yet in Redis
docker compose exec valkey valkey-cli lrange goflow:jobs:pending 0 -1               # job IDs waiting
docker compose exec valkey valkey-cli lrange goflow:jobs:processing 0 -1            # taken, not finished
docker compose exec valkey valkey-cli zrange goflow:jobs:delayed 0 -1 withscores    # retries and when they're due (Unix ms)
docker compose exec postgres psql -U goflow -c "SELECT id, status, attempts, next_attempt_at, last_error FROM jobs WHERE status IN ('retrying','dead')"
```

## Deploying

The hosted API deploys itself from `main` on Render. Setup, settings, measurements and limits are in [DEPLOYMENT.md](DEPLOYMENT.md).

## Debugging

- **Stack traces:** read top to bottom. The top frame is where it failed.
- **Race reports:** read both stacks (`Read at` / `Previous write at`) and the `created at` lines.
- **A user's failed request:** search the API logs for their `X-Request-ID`.
- **A quiet command:** check `echo $?`. Add `-w '%{http_code}\n'` to quiet `curl` calls.
- **A frozen command:** it may be reading stdin (e.g. `gofmt -l` with no path). Press `Ctrl+C`.
- **Measuring speed:** same command, several runs each, before and after.

## Common mistakes

| Symptom | Cause |
|---|---|
| `imported and not used` / `declared and not used` | Go rejects both. Organize imports on save |
| `expected declaration, found 'package'` | A file was pasted into another instead of replacing it. Select all before pasting |
| `undefined: x` | Wrong scope, or a missing import in that file |
| `network is unreachable` on an IPv6 address | The network has no IPv6 route. Disable IPv6 for that connection |
| `Permission denied (publickey)` | This machine's SSH key isn't on your GitHub account |
| `curl -d` returns 415 | Add `-H 'Content-Type: application/json'` |
| `cannot reach the GoFlow API` | The API isn't running, or `GOFLOW_API_URL` is wrong |
| Database tests are skipped | `GOFLOW_TEST_DATABASE_URL` isn't set in this shell |
| `registration is closed` (403) | `GOFLOW_REGISTRATION` isn't `open` for this server. That is the default on purpose |
| `error: ...` then `run: goflow login` | The API answered 401: no saved login for this `GOFLOW_API_URL`, or the session expired after 24 hours or was logged out |
| `hostname resolving error: ... i/o timeout` at startup | DNS on this network dropped or delayed the lookup (seen on a phone hotspot). Retry, or see FAILURES.md, Environment incidents |
| `event not found` in zsh | `!` inside double quotes triggers history expansion. Use single quotes, or run the script with `bash file.sh` |
