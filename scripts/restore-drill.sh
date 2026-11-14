#!/usr/bin/env bash
# Restore drill (Phase 16): proves an encrypted backup can be read back.
# A backup counts only once it has been restored.
#
#   ./scripts/restore-drill.sh ~/goflow-backups/goflow-<time>.dump.gpg
#
# What it does, and nothing else:
#   - decrypts the file with gpg (asks for the passphrase) and turns the
#     dump into SQL with pg_restore -f -, in memory: the decrypted dump
#     never touches the disk
#   - checks ALL of that SQL with scripts/restore-filter.awk before psql
#     sees any of it: the dump must be complete, and the only lines removed
#     are Neon's "ALTER COLUMN ... SET (pg_attver=N);" options, which stock
#     PostgreSQL rejects. They are printed. Anything unexpected stops the
#     drill with nothing restored (Task 69b; FAILURES.md, case 65)
#   - only then creates a NEW local database, goflow_restore_drill, in the
#     Compose PostgreSQL (docker compose up -d --wait postgres first), and
#     loads the SQL with psql: one transaction, stopping at the first
#     error, so all of it lands or none does. It refuses if that database
#     already exists
#   - checks that the foreign keys, indexes and CHECK constraints in the
#     dump all exist in the restored database, by asking PostgreSQL's
#     catalogs
#   - prints the row count of every GoFlow table and how long it took
#   - leaves the database for you to look at; drop it with
#       docker compose exec -T postgres dropdb -U goflow goflow_restore_drill
#
# It restores into the local Compose PostgreSQL only. There is no option
# for another destination: restoring into a real database is the manual
# runbook in DEPLOYMENT.md (Disaster recovery), done step by step on purpose.
#
# The whole dump is held in memory while it's checked: fine for GoFlow's
# size (Neon Free caps the database at 1 GB).
#
# Tests only: GOFLOW_PG_TOOLS=local uses this machine's psql and
# pg_restore against GOFLOW_DRILL_DATABASE_URL (a URL of a local server's
# "postgres" database, on localhost), GOFLOW_DRILL_DB names the database
# (it must start with goflow_restore_drill), and GOFLOW_GPG_PASSFILE reads
# the passphrase from a file.
set -euo pipefail

file="${1:-}"
[ -n "$file" ] && [ -f "$file" ] || { echo "usage: $0 <backup.dump.gpg>" >&2; exit 2; }
command -v gpg >/dev/null || { echo "stop: gpg is not installed" >&2; exit 1; }
command -v awk >/dev/null || { echo "stop: awk is not installed" >&2; exit 1; }
here="$(cd "$(dirname "$0")" && pwd)"
tools="${GOFLOW_PG_TOOLS:-docker}"
db=goflow_restore_drill

gpg_args=(--decrypt --quiet)
if [ -n "${GOFLOW_GPG_PASSFILE:-}" ]; then
  gpg_args=(--batch --pinentry-mode loopback --passphrase-file "$GOFLOW_GPG_PASSFILE" "${gpg_args[@]}")
fi

if [ "$tools" = docker ]; then
  cd "$here/.."
  psql_() { docker compose exec -T postgres psql -U goflow -d "$1" -X -tAc "$2" </dev/null; }
  to_sql_() { docker compose exec -T postgres pg_restore --no-owner --no-privileges -f -; }
  load_() { docker compose exec -T postgres psql -U goflow -d "$db" -X -q -v ON_ERROR_STOP=1 --single-transaction >/dev/null; }
  create_() { docker compose exec -T postgres createdb -U goflow "$db" </dev/null; }
else
  base="${GOFLOW_DRILL_DATABASE_URL:?set GOFLOW_DRILL_DATABASE_URL for GOFLOW_PG_TOOLS=local}"
  case "$base" in
    postgres://*@localhost[:/]*|postgres://*@127.0.0.1[:/]*) ;;
    *) echo "stop: GOFLOW_DRILL_DATABASE_URL must be a local server (localhost or 127.0.0.1)" >&2; exit 1 ;;
  esac
  db="${GOFLOW_DRILL_DB:-$db}"
  case "$db" in
    goflow_restore_drill|goflow_restore_drill_[a-z0-9_]*) ;;
    *) echo "stop: GOFLOW_DRILL_DB must start with goflow_restore_drill" >&2; exit 1 ;;
  esac
  dburl() { echo "${base%/*}/$1${base##*/postgres}"; }
  psql_() { psql "$(dburl "$1")" -X -tAc "$2" </dev/null; }
  to_sql_() { pg_restore --no-owner --no-privileges -f -; }
  load_() { psql "$(dburl "$db")" -X -q -v ON_ERROR_STOP=1 --single-transaction >/dev/null; }
  create_() { psql_ postgres "CREATE DATABASE $db" >/dev/null; }
fi

if [ "$(psql_ postgres "SELECT count(*) FROM pg_database WHERE datname = '$db'")" != 0 ]; then
  echo "stop: $db already exists; drop it first (see the top of this script)" >&2; exit 1
fi

start=$(date +%s)
echo "==> decrypting and checking the whole dump (gpg will ask for the passphrase)"
# Every stage must succeed (pipefail): a failed gpg or pg_restore, or a
# failed check, stops here, before any database is created.
if ! sql="$(gpg "${gpg_args[@]}" "$file" | to_sql_ | awk -f "$here/restore-filter.awk")"; then
  echo "stop: the dump didn't pass its checks; nothing was restored" >&2; exit 1
fi
counts="${sql##*$'\n'}"   # the filter's last line: -- goflow-drill: removed=N ...
case "$counts" in
  "-- goflow-drill: removed="*) ;;
  *) echo "stop: the filter's summary line is missing; nothing was restored" >&2; exit 1 ;;
esac
want_fk=$(printf '%s\n' "$counts" | sed -n 's/.* foreign_keys=\([0-9]*\).*/\1/p')
want_idx=$(printf '%s\n' "$counts" | sed -n 's/.* indexes=\([0-9]*\).*/\1/p')
want_chk=$(printf '%s\n' "$counts" | sed -n 's/.* checks=\([0-9]*\).*/\1/p')
echo "==> checked: ${counts#-- goflow-drill: }"

echo "==> restoring into $db (one transaction)"
create_
printf '%s\n' "$sql" | load_
unset sql
took=$(( $(date +%s) - start ))

catalog="SELECT
  (SELECT count(*) FROM pg_constraint c JOIN pg_namespace n ON n.oid = c.connamespace WHERE n.nspname = 'public' AND c.contype = 'f')
  || ' ' || (SELECT count(*) FROM pg_indexes WHERE schemaname = 'public')
  || ' ' || (SELECT count(*) FROM pg_constraint c JOIN pg_namespace n ON n.oid = c.connamespace WHERE n.nspname = 'public' AND c.contype = 'c')"
read -r got_fk got_idx got_chk <<<"$(psql_ "$db" "$catalog")"
echo "==> structure (in the dump / in $db):"
printf '  %-18s %s / %s\n' "foreign keys" "$want_fk" "$got_fk" "indexes" "$want_idx" "$got_idx" "CHECK constraints" "$want_chk" "$got_chk"
if [ "$want_fk" != "$got_fk" ] || [ "$want_idx" != "$got_idx" ] || [ "$want_chk" != "$got_chk" ]; then
  echo "stop: $db is missing structure the dump has; look at it before dropping it" >&2; exit 1
fi

echo "==> restored into $db in ${took}s; rows per table:"
for t in users sessions jobs job_outbox webhook_secrets webhook_attempts schema_migrations; do
  printf '  %-18s %s\n' "$t" "$(psql_ "$db" "SELECT count(*) FROM $t" 2>/dev/null || echo missing)"
done
printf '  %-18s %s\n' "newest job" "$(psql_ "$db" "SELECT coalesce(to_char(max(created_at) AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS'), '-') FROM jobs" 2>/dev/null || echo missing)"
echo "Compare with production (./scripts/count-rows.sh), then drop $db."
