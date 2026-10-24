#!/usr/bin/env bash
# Runs every pre-commit check and stops at the first failure, saying which one.
set -euo pipefail

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

echo "ALL CHECKS PASSED"