#!/usr/bin/env bash
# Row counts of GoFlow's tables in one database, read-only (Phase 16).
# The restore drill runs it against production and against the Neon branch
# restored from a past moment, and compares.
#
#   ./scripts/count-rows.sh           asks for the database URL (not shown)
#
# It runs psql from the postgres:17 image (GOFLOW_PG_TOOLS=local: this
# machine's psql) inside a read-only transaction, checks the server's
# certificate and name against this machine's CA bundle (sslmode=verify-full,
# scripts/pgurl.sh), and never prints the URL.
set -euo pipefail

tools="${GOFLOW_PG_TOOLS:-docker}"
ca=/etc/ssl/certs/ca-certificates.crt
# shellcheck source=scripts/pgurl.sh
. "$(dirname "$0")/pgurl.sh"
url="${GOFLOW_COUNT_DATABASE_URL:-}"
if [ -z "$url" ]; then
  read -rsp "Database URL (not shown): " url
  echo
fi
case "$url" in
  postgres://*|postgresql://*) ;;
  *) echo "stop: that isn't a postgres:// URL" >&2; exit 1 ;;
esac

sql="BEGIN READ ONLY;
SELECT 'users', count(*) FROM users
UNION ALL SELECT 'sessions', count(*) FROM sessions
UNION ALL SELECT 'jobs', count(*) FROM jobs
UNION ALL SELECT 'jobs completed', count(*) FROM jobs WHERE status = 'completed'
UNION ALL SELECT 'webhook_secrets', count(*) FROM webhook_secrets
UNION ALL SELECT 'webhook_attempts', count(*) FROM webhook_attempts
UNION ALL SELECT 'schema_migrations', count(*) FROM schema_migrations;
SELECT 'newest job created', coalesce(to_char(max(created_at) AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS'), '-') FROM jobs;
COMMIT;"

if [ "$tools" = docker ]; then
  PGURL="$(verified_url "$url" /etc/goflow-ca.crt)" PGSQL="$sql" \
    docker run --rm -i -e PGURL -e PGSQL -v "$ca:/etc/goflow-ca.crt:ro" postgres:17 \
      sh -c 'exec psql --dbname="$PGURL" -X -q -tA -F " | " -v ON_ERROR_STOP=1 -c "$PGSQL"' </dev/null
else
  psql --dbname="$(verified_url "$url" "$ca")" -X -q -tA -F " | " -v ON_ERROR_STOP=1 -c "$sql" </dev/null
fi
