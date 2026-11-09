#!/usr/bin/env bash
# Runs every pre-commit check and stops at the first failure, saying which one.
#
# Tests that need PostgreSQL or Redis skip themselves when their URL isn't
# set, and a skipped test prints "ok" like a passing one. So a full check
# refuses to start without both URLs (Task 49: the Redis tests had been
# skipping on the development laptop since Phase 5, unnoticed).
# GOFLOW_QUICK_CHECK=1 runs anyway, and says so at the end.
set -euo pipefail

quick=0
if [ "${GOFLOW_QUICK_CHECK:-}" = "1" ]; then
	quick=1
	echo "==> QUICK CHECK: tests that need PostgreSQL or Redis skip if their URL is missing"
else
	missing=""
	[ -n "${GOFLOW_TEST_DATABASE_URL:-}" ] || missing="$missing GOFLOW_TEST_DATABASE_URL"
	[ -n "${GOFLOW_TEST_REDIS_URL:-}" ] || missing="$missing GOFLOW_TEST_REDIS_URL"
	if [ -n "$missing" ]; then
		echo "check.sh: not set:$missing" >&2
		echo "The tests that need them would be skipped, and a skipped test looks like a pass." >&2
		echo "Set them (DEVELOPMENT.md, Environment), or run a quick check that skips them:" >&2
		echo "  GOFLOW_QUICK_CHECK=1 ./scripts/check.sh" >&2
		exit 1
	fi
fi

echo "==> gofmt"
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
	echo "Not formatted (run: gofmt -w .):"
	echo "$unformatted"
	exit 1
fi

echo "==> go vet"
go vet ./...

echo "==> go test -race"
go test -race -count=1 ./...

if [ "$quick" = "1" ]; then
	echo "QUICK CHECK PASSED (tests needing PostgreSQL or Redis may have been skipped: not a full check)"
else
	echo "ALL CHECKS PASSED"
fi
