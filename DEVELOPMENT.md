# Development

## Prerequisites

- Go 1.27 or newer
- `goimports`: `go install golang.org/x/tools/cmd/goimports@latest`
- `jq` for reading JSON in the terminal
- Make sure `$(go env GOPATH)/bin` is on your `PATH`

### VS Code

Install the official Go extension, then add this to your user settings JSON so every save formats the file and fixes imports:

```json
"[go]": {
    "editor.formatOnSave": true,
    "editor.codeActionsOnSave": {
        "source.organizeImports": "explicit"
    }
}
```

## Workflow

1. Start from an up-to-date `main`:
   ```bash
   git checkout main && git pull
   git checkout -b feat/short-description
   ```
   Branch prefixes: `feat/`, `fix/`, `test/`, `docs/`, `chore/`.
2. Make the change, with tests.
3. Run every check, then commit:
   ```bash
   ./scripts/check.sh && git add . && git status && git commit -m "feat(scope): what changed"
   ```
   Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/): `feat`, `fix`, `test`, `docs`, `chore`, `refactor`.
4. Push and open a pull request. **If the PR has more than one commit, edit its title** to a conventional commit message; GitHub otherwise uses the branch name.
5. Merge with **Squash and merge**.
6. Verify, and only then delete the branch:
   ```bash
   git checkout main && git pull && ./scripts/check.sh && git log --oneline -3
   git branch -D feat/short-description
   git push origin --delete feat/short-description
   git fetch --prune
   ```

## Checks

`./scripts/check.sh` runs, in order, and stops at the first failure:

1. `gofmt -l .` (fails if any file is unformatted)
2. `go vet ./...`
3. `go test -race -count=1 ./...`

## Tests

| Kind | Where | What it covers |
|---|---|---|
| Unit | `internal/job` | validation, store, persistence |
| Unit | `internal/cli` | commands via `cli.Run` with in-memory output |
| Handler | `internal/api` | every endpoint and middleware via `httptest` |
| Concurrency | `*_concurrency_test.go` | 100 goroutines against the store and the API |
| End-to-end | `cmd/api` | real server on `127.0.0.1:0`, real HTTP, graceful shutdown |

Useful commands:

```bash
go test -race -count=5 ./...         # repeat to shake out intermittent races
go test -run TestCreateJob -v ./internal/api/   # one test, verbose
```

### Breaking things on purpose

To see what a test protects against, **commit first**, then break the code, run the test, and restore with:

```bash
git checkout -- path/to/file.go
```

`git checkout --` only restores files Git already knows about. Untracked files must be fixed by hand.

## Running locally

### API

```bash
go build -o bin/api ./cmd/api && ./bin/api
```

```bash
curl -s -X POST http://localhost:8080/api/v1/jobs \
  -H 'Content-Type: application/json' -d '{"title":"send email"}' | jq
curl -s http://localhost:8080/api/v1/jobs | jq
```

### Load test with the race detector built in

```bash
go build -race -o bin/api ./cmd/api && ./bin/api
```

```bash
seq 500 | xargs -P 50 -I{} curl -s -o /dev/null -w "%{http_code}\n" \
  -X POST http://localhost:8080/api/v1/jobs \
  -H 'Content-Type: application/json' -d '{"title":"load job {}"}' | sort | uniq -c
```

Expect `500 201` and no `WARNING: DATA RACE` in the server output.

### CLI

```bash
go build -o bin/goflow ./cmd/goflow
GOFLOW_DATA_FILE=/tmp/goflow-dev.json ./bin/goflow job list
```

## Debugging

- **Stack traces:** read top to bottom. The top frame is where it failed; each line below is its caller.
- **Race reports:** look at both sides, the access (`Read at`/`Write at`) and the `Previous` access. Both stacks name a file and line. The `created at` lines show where each goroutine was started.
- **A user's failed request:** ask for the `X-Request-ID` response header and search the server logs for it.
- **A command printed nothing:** check its exit code with `echo $?`. Some tools (`test`, `sed`) are silent on failure.
- **A command seems frozen:** it may be waiting for input on stdin (for example `gofmt -l` without a path). Press `Ctrl+C`.

## Common mistakes

| Symptom | Cause |
|---|---|
| `imported and not used` | Go rejects unused imports; enable organize-imports on save |
| `declared and not used` | Go rejects unused local variables |
| `undefined: x` | `x` is declared in another function's scope or the import is missing |
| `gofmt -l` lists files after pasting code | Pasted spaces instead of tabs; run `gofmt -w .` |
| `curl -d` returns 415 | `curl -d` sends form encoding; add `-H 'Content-Type: application/json'` |
| `Permission denied (publickey)` | This machine's SSH key isn't on your GitHub account |