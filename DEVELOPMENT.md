# Development

## Prerequisites

- Go 1.27 or newer
- Docker (for PostgreSQL)
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

### PostgreSQL

```bash
docker run -d --name goflow-postgres \
  -e POSTGRES_USER=goflow -e POSTGRES_PASSWORD=goflow -e POSTGRES_DB=goflow \
  -p 127.0.0.1:5432:5432 -v goflow-pgdata:/var/lib/postgresql/data \
  postgres:17
until docker exec goflow-postgres pg_isready -U goflow -q; do sleep 1; done
docker exec goflow-postgres psql -U goflow -c 'CREATE DATABASE goflow_test'
```

Bind to `127.0.0.1` only: Docker's port rules bypass most host firewalls. The container has no restart policy, so after a reboot run `docker start goflow-postgres`.

### Environment

```bash
export GOFLOW_DATABASE_URL='postgres://goflow:goflow@127.0.0.1:5432/goflow?sslmode=disable'
export GOFLOW_TEST_DATABASE_URL='postgres://goflow:goflow@127.0.0.1:5432/goflow_test?sslmode=disable'
```

| Variable | Used by | Default |
|---|---|---|
| `GOFLOW_DATABASE_URL` | API (required) | none |
| `GOFLOW_ADDR` | API | `:8080` |
| `GOFLOW_TEST_DATABASE_URL` | tests; database tests skip without it | none |
| `GOFLOW_API_URL` | CLI | `http://localhost:8080` |

Tests must never point at the development database.

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
go build -o bin/api ./cmd/api && ./bin/api           # terminal 1
go build -o bin/goflow ./cmd/goflow                  # terminal 2
id=$(./bin/goflow job create send welcome email)
./bin/goflow job get "$id"
curl -s "localhost:8080/api/v1/jobs?limit=2" | jq
```

Look at the data directly:

```bash
docker exec goflow-postgres psql -U goflow -c 'SELECT id, title, status FROM jobs'
```

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