#!/usr/bin/env bash
# Restore drill (Phase 16): proves an encrypted backup can be read back.
# A backup counts only once it has been restored.
#
#   ./scripts/restore-drill.sh ~/goflow-backups/goflow-<time>.dump.gpg
#
# What it does, and nothing else:
#   - decrypts the file with gpg (asks for the passphrase) straight into
#     pg_restore: the plain dump never touches the disk
#   - restores it into a NEW local database, goflow_restore_drill, in the
#     Compose PostgreSQL (docker compose up -d --wait postgres first). It
#     refuses if that database already exists, and never touches Neon,
#     goflow or goflow_test
#   - prints the row count of every GoFlow table and how long it took
#   - leaves the database for you to look at; drop it with
#       docker compose exec -T postgres dropdb -U goflow goflow_restore_drill
#
# Tests only: GOFLOW_PG_TOOLS=local uses this machine's psql and
# pg_restore against GOFLOW_DRILL_DATABASE_URL (a URL of the server's
# "postgres" database), and GOFLOW_GPG_PASSFILE reads the passphrase from
# a file.
set -euo pipefail

file="${1:-}"
[ -n "$file" ] && [ -f "$file" ] || { echo "usage: $0 <backup.dump.gpg>" >&2; exit 2; }
command -v gpg >/dev/null || { echo "stop: gpg is not installed" >&2; exit 1; }
tools="${GOFLOW_PG_TOOLS:-docker}"
db=goflow_restore_drill

gpg_args=(--decrypt --quiet)
if [ -n "${GOFLOW_GPG_PASSFILE:-}" ]; then
  gpg_args=(--batch --pinentry-mode loopback --passphrase-file "$GOFLOW_GPG_PASSFILE" "${gpg_args[@]}")
fi

if [ "$tools" = docker ]; then
  cd "$(dirname "$0")/.."
  psql_() { docker compose exec -T postgres psql -U goflow -d "$1" -tAc "$2" </dev/null; }
  restore_() { docker compose exec -T postgres pg_restore --no-owner --no-privileges --exit-on-error -U goflow -d "$db"; }
  create_() { docker compose exec -T postgres createdb -U goflow "$db" </dev/null; }
else
  base="${GOFLOW_DRILL_DATABASE_URL:?set GOFLOW_DRILL_DATABASE_URL for GOFLOW_PG_TOOLS=local}"
  dburl() { echo "${base%/*}/$1${base##*/postgres}"; }
  psql_() { psql "$(dburl "$1")" -tAc "$2" </dev/null; }
  restore_() { pg_restore --no-owner --no-privileges --exit-on-error -d "$(dburl "$db")"; }
  create_() { psql_ postgres "CREATE DATABASE $db" >/dev/null; }
fi

if [ "$(psql_ postgres "SELECT count(*) FROM pg_database WHERE datname = '$db'")" != 0 ]; then
  echo "stop: $db already exists; drop it first (see the top of this script)" >&2; exit 1
fi

start=$(date +%s)
create_
echo "==> decrypting into pg_restore (gpg will ask for the passphrase)"
gpg "${gpg_args[@]}" "$file" | restore_
took=$(( $(date +%s) - start ))

echo "==> restored into $db in ${took}s; rows per table:"
for t in users sessions jobs job_outbox webhook_secrets webhook_attempts schema_migrations; do
  printf '  %-18s %s\n' "$t" "$(psql_ "$db" "SELECT count(*) FROM $t" 2>/dev/null || echo missing)"
done
echo "Compare with production (DEPLOYMENT.md, restore drill), then drop $db."
