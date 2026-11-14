#!/usr/bin/env bash
# Runs GoFlow's benchmarks and summarises them with benchstat (Task 57).
#
#   ./scripts/bench.sh                    # 10 runs of each benchmark
#   GOFLOW_BENCH_COUNT=3 ./scripts/bench.sh
#
# Needs PostgreSQL (docker compose up -d --wait postgres valkey),
# GOFLOW_TEST_DATABASE_URL, and benchstat on PATH (DEVELOPMENT.md,
# Benchmarks). Raw results go to bin/bench-<time>.txt, which git ignores.
# Not part of check.sh: benchmarks take minutes and their numbers depend on
# the machine. check.sh compiles them, so they can't rot.
set -euo pipefail
cd "$(dirname "$0")/.."

count=${GOFLOW_BENCH_COUNT:-10}
if ! command -v benchstat >/dev/null; then
	echo "stop: benchstat isn't installed: see DEVELOPMENT.md, Benchmarks" >&2
	exit 1
fi
if [ -z "${GOFLOW_TEST_DATABASE_URL:-}" ]; then
	echo "stop: GOFLOW_TEST_DATABASE_URL isn't set: the database benchmarks need it" >&2
	exit 1
fi

mkdir -p bin
stamp=$(date +%Y%m%d-%H%M%S)
db=bin/bench-$stamp-database.txt
mem=bin/bench-$stamp-memorystore.txt

# Everything a reader needs to repeat or doubt the numbers.
header() {
	echo "# GoFlow benchmarks, $(date -u +%Y-%m-%dT%H:%M:%SZ), commit $(git rev-parse --short HEAD)"
	echo "# $(go version)"
	echo "# os: $(uname -srm)"
	echo "# cpu: $(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2- | sed 's/^ *//') ($(nproc) threads)"
	echo "# postgres: $(docker compose exec -T postgres psql -U goflow -tAc 'SHOW server_version' </dev/null 2>/dev/null || echo unknown)"
	echo "# benchstat: $(go version -m "$(command -v benchstat)" | awk '$1 == "mod" { print $3 }')"
}

echo "==> database benchmarks, $count runs each (a few minutes)"
{
	header
	echo "# command: go test -run '^\$' -bench . -benchmem -count=$count ./internal/database/"
} >"$db"
go test -run '^$' -bench . -benchmem -count="$count" ./internal/database/ | tee -a "$db"

echo "==> MemoryStore Lock vs RLock, $count runs each, 1 and 8 readers"
{
	header
	echo "# command: go test -run '^\$' -bench MemoryStoreGet -benchmem -count=$count -cpu 1,8 ./internal/job/"
} >"$mem"
go test -run '^$' -bench MemoryStoreGet -benchmem -count="$count" -cpu 1,8 ./internal/job/ | tee -a "$mem"

echo
echo "==> summary: database"
head -6 "$db"
benchstat "$db"
echo
echo "==> summary: Lock (columns) against RLock, by number of readers (rows)"
benchstat -col /lock "$mem"
echo
echo "raw results: $db and $mem"
