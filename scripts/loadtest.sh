#!/usr/bin/env bash
# Measures jobs per second on the local Compose stack with 1, 3 and 6 worker
# containers (Task 56). Local only: cmd/loadgen refuses any API that isn't
# on this machine.
#
# Before: docker compose up -d --wait, then log in to the local API once
# (./bin/goflow login). Loadgen uses that saved login.
#
# For each step it recreates the api and worker containers with the API's
# own workers off (GOFLOW_API_WORKERS=0), so only worker containers run
# jobs, then counts the workers that actually started from their logs and
# passes that number to the report. PostgreSQL and Valkey keep running and
# keep their data. At the end the stack goes back to its usual shape: three
# worker containers, API workers on.
#
#   ./scripts/loadtest.sh                 # 500 jobs, 3 runs per step
#   GOFLOW_LOADTEST_JOBS=100 GOFLOW_LOADTEST_RUNS=1 ./scripts/loadtest.sh
#   GOFLOW_LOADTEST_CONTAINERS="3 6" GOFLOW_LOADTEST_JOBS=2000 GOFLOW_LOADTEST_STATS=1 ./scripts/loadtest.sh
#
# GOFLOW_LOADTEST_STATS=1 takes one `docker stats` reading of every
# container's CPU half a second into each run: one sample, a clue, not a
# measurement. Use enough jobs that a run lasts a few seconds.
set -euo pipefail
cd "$(dirname "$0")/.."

jobs=${GOFLOW_LOADTEST_JOBS:-500}
runs=${GOFLOW_LOADTEST_RUNS:-3}
steps=${GOFLOW_LOADTEST_CONTAINERS:-1 3 6}
stats=${GOFLOW_LOADTEST_STATS:-0}

go build -o bin/loadgen ./cmd/loadgen

# A first job on the stack as it is, before anything is recreated: a missing
# or expired login, or a stack that isn't up, stops the script here.
# Loadgen exits 2 when it couldn't run at all, 1 when jobs went wrong.
echo "==> preflight: one job"
if ! ./bin/loadgen -jobs 1 -wait 30s -label preflight >/dev/null; then
	echo "stop: loadgen couldn't run one job. Is the stack up (docker compose up -d --wait) and are you logged in (./bin/goflow login)?" >&2
	exit 1
fi

# started_workers prints how many workers the running api and worker
# containers started, from each pool's "worker pool started workers=N".
started_workers() {
	docker compose logs --no-log-prefix api worker 2>/dev/null |
		sed -n 's/.*msg="worker pool started" workers=\([0-9][0-9]*\).*/\1/p' |
		awk '{ s += $1 } END { print s + 0 }'
}

# started_pools counts those lines: one per process whose pool is running.
started_pools() {
	docker compose logs --no-log-prefix api worker 2>/dev/null | grep -c 'msg="worker pool started"' || true
}

for containers in $steps; do
	echo "==> $containers worker container(s), API workers off"
	GOFLOW_API_WORKERS=0 docker compose up -d --wait --force-recreate --scale worker="$containers" api worker

	# Wait until every container's pool has started (startup recovery
	# needs PostgreSQL and Redis), at most 60 s.
	for _ in $(seq 60); do
		[ "$(started_pools)" -ge "$containers" ] && break
		sleep 1
	done
	pools=$(started_pools)
	workers=$(started_workers)
	if [ "$pools" -ne "$containers" ]; then
		echo "stop: $pools worker pools started, want $containers (docker compose logs worker)" >&2
		exit 1
	fi
	echo "    $pools pools, $workers workers"

	for run in $(seq "$runs"); do
		echo "--- run $run of $runs"
		status=0
		if [ "$stats" = "1" ]; then
			./bin/loadgen -jobs "$jobs" -workers "$workers" -label "$containers containers, run $run" >/tmp/goflow-loadgen.out &
			pid=$!
			sleep 0.5
			echo "    cpu, one reading 0.5 s in:"
			docker stats --no-stream --format '      {{.Name}} {{.CPUPerc}}' $(docker compose ps -q) </dev/null || true
			wait "$pid" || status=$?
			cat /tmp/goflow-loadgen.out
		else
			./bin/loadgen -jobs "$jobs" -workers "$workers" -label "$containers containers, run $run" || status=$?
		fi
		case $status in
		0) ;;
		1) echo "    (this run had jobs that failed or didn't finish: see above)" ;;
		*)
			echo "stop: loadgen couldn't run (exit $status): see above" >&2
			exit 1
			;;
		esac
	done
done

echo "==> back to the usual stack: 3 worker containers, API workers on"
docker compose up -d --wait --force-recreate --scale worker=3 api worker
