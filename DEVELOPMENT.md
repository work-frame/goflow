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

## Tests

```bash
go test -count=1 ./...                               # everything
go test -count=1 -v -run TestJobStore ./internal/database/
go test -count=5 -race ./...                         # shake out intermittent races
go test -count=1 -timeout 10s ./internal/cli/        # fail fast if something loops
```

See the Testing section of [ARCHITECTURE.md](ARCHITECTURE.md) for what each package's tests cover.

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
