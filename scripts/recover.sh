#!/usr/bin/env bash
# Disaster recovery (Task 81): loads an encrypted backup into a NEW, EMPTY
# database, checks it, and times every step.
#
#   ./scripts/recover.sh ~/Downloads/goflow-<time>.dump.age
#
# It asks, in this order:
#   1. when you decided to restore (UTC HH:MM, from `date -u`; Enter skips):
#      the RTO clock starts there, not when this script starts
#   2. the TARGET URL (hidden): the neondb_owner URL of the new Neon
#      project, pooling off
#   3. that URL's endpoint ID, typed by you from the Neon console of the
#      new project (ep-...): proof the URL belongs to the project you mean
#   4. the age private key (hidden), only after the target is checked
#   5. whether to start GoFlow locally against the restored database, and
#      if so GOFLOW_SECRETS_KEY (hidden), to log in and see the jobs
#
# What it refuses:
#   - a target with ANY table, view or sequence in schema public, or a
#     schema_migrations table anywhere: it never loads over data, so the
#     production database can't be its target
#   - a pooled URL, and a URL whose endpoint isn't the one you typed
#   - a dump that fails scripts/restore-filter.awk (cut off, or anything
#     unexpected): nothing is sent to the target
# The load is one transaction that stops at the first error: all of the
# backup lands, or none of it. Then the foreign keys, indexes and CHECK
# constraints are counted in the target's catalogs and must equal the
# dump's, and the rows per table are printed.
#
# Secrets: the URL, its password and both keys are read without echo and
# never printed or written to disk. The password reaches psql as
# PGPASSWORD, not on a command line. The backup's SHA-1 is printed to catch
# a damaged download; it is NOT proof the backup is genuine (DEPLOYMENT.md,
# What the checksum proves).
#
# It doesn't touch Render. Pointing the hosted API at the new database is
# the runbook's next step (DEPLOYMENT.md, Disaster recovery), done by hand.
#
# Tests only: GOFLOW_PG_TOOLS=local uses this machine's psql and
# pg_restore, and then the target must be on localhost or 127.0.0.1.
set -euo pipefail

file="${1:-}"
[ -n "$file" ] && [ -f "$file" ] || { echo "usage: $0 <backup.dump.age or .gpg>" >&2; exit 2; }
here="$(cd "$(dirname "$0")" && pwd)"
. "$here/pgurl.sh"
. "$here/keyid.sh"
tools="${GOFLOW_PG_TOOLS:-docker}"
ca=/etc/ssl/certs/ca-certificates.crt
api_name=goflow-recover-api
api_port=8081

stop() { echo "stop: $*" >&2; exit 1; }
for c in awk sha1sum base64 sha256sum od; do command -v "$c" >/dev/null || stop "$c is not installed"; done
case "$file" in
  *.age) command -v age >/dev/null || stop "age is not installed" ;;
  *.gpg) command -v gpg >/dev/null || stop "gpg is not installed" ;;
  *) echo "stop: $file is neither .age nor .gpg" >&2; exit 2 ;;
esac
case "$tools" in
  docker)
    command -v docker >/dev/null || stop "docker is not installed"
    [ -r "$ca" ] || stop "no CA bundle at $ca (needed to check the server's certificate)" ;;
  local)
    command -v psql >/dev/null && command -v pg_restore >/dev/null || stop "psql and pg_restore are needed for GOFLOW_PG_TOOLS=local" ;;
  *) stop "GOFLOW_PG_TOOLS must be docker or local" ;;
esac

# The clock. mark NAME records how long the step that just ended took.
start=$(date -u +%s)
last=$start
steps=()
mark() {
  local now
  now=$(date -u +%s)
  steps+=("$(printf '  %-40s %6ss   done %s UTC' "$1" "$((now - last))" "$(date -u -d "@$now" +%H:%M:%S)")")
  last=$now
  printf '[%s UTC] %s\n' "$(date -u -d "@$now" +%H:%M:%S)" "$1"
}

decided=""
read -rp "When did you decide to restore? UTC, HH:MM (Enter to skip): " decided || true
decided=${decided//[[:space:]]/}
if [ -n "$decided" ] && ! [[ "$decided" =~ ^([01][0-9]|2[0-3]):[0-5][0-9]$ ]]; then
  stop "give the time as HH:MM in UTC, like 19:05 (date -u shows it)"
fi

sha1=$(sha1sum "$file" | cut -c1-40)
echo "backup: $(basename "$file"), $(wc -c <"$file" | tr -d ' ') bytes, sha1 $sha1"
echo "  (compare with the backup run's log: a match rules out a damaged download, not a forged file)"

# --- The target -----------------------------------------------------------
url=""
read -rsp "TARGET database URL: the NEW project's neondb_owner URL, pooling off (paste, Enter; not shown): " url || true
echo
url=${url//[[:space:]]/}
url=${url#psql}; url=${url#\'}; url=${url%\'}   # Neon's "psql '...'" snippet, if copied whole
case "$url" in
  postgres://*@*/*|postgresql://*@*/*) ;;
  *) n=${#url}; unset url; stop "that isn't a postgresql://user:password@host/database URL (got $n characters)" ;;
esac
case "$url" in *-pooler.*) unset url; stop "that's a pooled URL; in Connect, turn Connection pooling off and copy again" ;; esac

base="${url%%\?*}"
hostport="${base#*://}"; hostport="${hostport%%/*}"   # user:pass@host:port
creds="${hostport%@*}"                                 # user:pass
host="${hostport##*@}"; host="${host%%:*}"
user="${creds%%:*}"
pw=""; [ "$creds" != "$user" ] && pw="${creds#*:}"
endpoint="${host%%.*}"

if [ "$tools" = local ]; then
  case "$host" in localhost|127.0.0.1) ;; *) unset url pw; stop "with GOFLOW_PG_TOOLS=local the target must be localhost or 127.0.0.1" ;; esac
  target="$url"
else
  target="$(verified_url "$url" /etc/goflow-ca.crt)"
fi
unset url
# The password goes to psql as PGPASSWORD; the URL psql sees has none.
case "$pw" in *%*) pw=$(printf '%b' "${pw//%/\\x}") ;; esac
target_nopw="${target/"$creds@"/"$user@"}"
unset creds

typed=""
read -rp "Type the NEW project's endpoint ID from the Neon console (Branches > production > Computes, ep-...): " typed || true
typed=${typed//[[:space:]]/}
if [ -z "$typed" ] || [ "$typed" != "$endpoint" ]; then
  unset pw target target_nopw
  stop "that isn't the endpoint in the URL you pasted; nothing was touched"
fi

if [ "$tools" = docker ]; then
  psql_() { # psql_ ARGS...: psql on the target in postgres:17, the CA bundle mounted
    PGPASSWORD="$pw" PGURL="$target_nopw" docker run --rm -i -e PGPASSWORD -e PGURL \
      -v "$ca:/etc/goflow-ca.crt:ro" postgres:17 sh -c 'exec psql --dbname="$PGURL" "$@"' sh "$@"
  }
  to_sql_() { docker run --rm -i postgres:17 pg_restore --no-owner --no-privileges -f -; }
else
  psql_() { PGPASSWORD="$pw" psql --dbname="$target_nopw" "$@"; }
  to_sql_() { pg_restore --no-owner --no-privileges -f -; }
fi
q() { printf '%s\n' "$1" | psql_ -X -q -tA -v ON_ERROR_STOP=1; }   # one value or row, from stdin SQL

empty_sql="SELECT current_user || ' ' || current_database()
  || ' ' || (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
             WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f'))
  || ' ' || (SELECT count(*) FROM pg_tables WHERE tablename = 'schema_migrations');"
if ! info=$(q "$empty_sql" 2>/dev/null); then
  stop "can't connect to the target (wrong URL, or Neon asleep: run it again)"
fi
read -r who dbname objects migrations <<<"$info"
echo "target: endpoint $endpoint, database $dbname, as $who: $objects objects in schema public, $migrations schema_migrations tables"
if [ "$objects" != 0 ] || [ "$migrations" != 0 ]; then
  stop "the target isn't empty; this script only loads into a NEW, EMPTY database. Nothing was changed"
fi
mark "target checked: empty"

# --- The backup -----------------------------------------------------------
case "$file" in
  *.age)
    identity=""
    read -rsp "age private key, AGE-SECRET-KEY-1... (not shown): " identity || true
    echo
    [[ "$identity" =~ ^AGE-SECRET-KEY-1[0-9A-Z]{58}$ ]] || { unset identity; stop "that isn't an age private key (one line, AGE-SECRET-KEY-1...)"; }
    # <(...) hands age a pipe, not a file: the key stays in this shell's memory.
    decrypt_() { age --decrypt --identity <(printf '%s\n' "$identity") "$file"; } ;;
  *.gpg)
    gpg_args=(--decrypt --quiet)
    [ -n "${GOFLOW_GPG_PASSFILE:-}" ] && gpg_args=(--batch --pinentry-mode loopback --passphrase-file "$GOFLOW_GPG_PASSFILE" "${gpg_args[@]}")
    decrypt_() { gpg "${gpg_args[@]}" "$file"; } ;;
esac
mark "key given"

echo "==> decrypting and checking the whole dump, in memory"
# Every stage must succeed (pipefail). Nothing reaches the target unless
# the whole dump decrypted and passed the filter.
if ! sql="$(decrypt_ | to_sql_ | awk -f "$here/restore-filter.awk")"; then
  unset identity pw target target_nopw
  stop "the dump didn't pass its checks; nothing was sent to the target"
fi
unset identity
counts="${sql##*$'\n'}"
case "$counts" in
  "-- goflow-drill: removed="*) ;;
  *) unset sql pw target target_nopw; stop "the filter's summary line is missing; nothing was sent to the target" ;;
esac
want_fk=$(printf '%s\n' "$counts" | sed -n 's/.* foreign_keys=\([0-9]*\).*/\1/p')
want_idx=$(printf '%s\n' "$counts" | sed -n 's/.* indexes=\([0-9]*\).*/\1/p')
want_chk=$(printf '%s\n' "$counts" | sed -n 's/.* checks=\([0-9]*\).*/\1/p')
echo "    ${counts#-- goflow-drill: }"
mark "backup decrypted and checked"

echo "==> loading into the target (one transaction)"
if ! printf '%s\n' "$sql" | psql_ -X -q -v ON_ERROR_STOP=1 --single-transaction >/dev/null; then
  unset sql pw target target_nopw
  stop "the load failed and was rolled back; the target is still empty"
fi
unset sql
mark "loaded"

catalog="SELECT
  (SELECT count(*) FROM pg_constraint c JOIN pg_namespace n ON n.oid = c.connamespace WHERE n.nspname = 'public' AND c.contype = 'f')
  || ' ' || (SELECT count(*) FROM pg_indexes WHERE schemaname = 'public')
  || ' ' || (SELECT count(*) FROM pg_constraint c JOIN pg_namespace n ON n.oid = c.connamespace WHERE n.nspname = 'public' AND c.contype = 'c');"
read -r got_fk got_idx got_chk <<<"$(q "$catalog")"
echo "==> structure (in the dump / in the target):"
printf '  %-18s %s / %s\n' "foreign keys" "$want_fk" "$got_fk" "indexes" "$want_idx" "$got_idx" "CHECK constraints" "$want_chk" "$got_chk"
if [ "$want_fk" != "$got_fk" ] || [ "$want_idx" != "$got_idx" ] || [ "$want_chk" != "$got_chk" ]; then
  unset pw target target_nopw
  stop "the target is missing structure the dump has; don't point anything at it"
fi
echo "==> rows per table:"
for t in users sessions jobs job_outbox webhook_secrets webhook_attempts schema_migrations; do
  printf '  %-18s %s\n' "$t" "$(q "SELECT count(*) FROM $t" 2>/dev/null || echo missing)"
done
printf '  %-18s %s\n' "newest job" "$(q "SELECT coalesce(to_char(max(created_at) AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS'), '-') FROM jobs" 2>/dev/null || echo missing)"
mark "structure and rows checked"

# --- Optional: GoFlow against the restored database ------------------------
ans=""
read -rp "Start GoFlow on http://127.0.0.1:$api_port against it, to log in and see the jobs? Type yes (Enter skips): " ans || true
if [ "$ans" = yes ]; then
  [ "$tools" = docker ] || stop "the local API needs docker (not GOFLOW_PG_TOOLS=local)"
  command -v curl >/dev/null || stop "curl is not installed"
  docker image inspect goflow:dev >/dev/null 2>&1 || stop "no goflow:dev image; build it first: docker compose build api"
  if docker container inspect "$api_name" >/dev/null 2>&1; then
    stop "a container named $api_name exists; remove it first: docker rm -f $api_name"
  fi
  skey=""
  read -rsp "GOFLOW_SECRETS_KEY from your password manager (not shown): " skey || true
  echo
  skey=${skey//[[:space:]]/}
  key_id=$(secrets_key_id "$skey") || { unset skey; stop "that can't be GoFlow's secrets key"; }
  echo "secrets key_id=$key_id (Render's log line 'secrets key loaded key_id=...' must show the same)"
  # The API needs the password inside its URL. Both values reach the
  # container as environment variables, never as arguments; while it
  # runs, `docker inspect` on this machine can show them. It is removed
  # when this script ends.
  trap 'docker rm -f "$api_name" >/dev/null 2>&1 || true' EXIT
  if ! GOFLOW_DATABASE_URL="$target" GOFLOW_SECRETS_KEY="$skey" docker run -d --name "$api_name" \
      -e GOFLOW_DATABASE_URL -e GOFLOW_SECRETS_KEY -e GOFLOW_REGISTRATION=closed \
      -p "127.0.0.1:$api_port:8080" -v "$ca:/etc/goflow-ca.crt:ro" goflow:dev >/dev/null; then
    unset skey; stop "docker couldn't start goflow:dev (is port $api_port free?)"
  fi
  unset skey
  ready=no
  for _ in $(seq 1 90); do
    if curl -fsS "http://127.0.0.1:$api_port/readyz" >/dev/null 2>&1; then ready=yes; break; fi
    [ "$(docker inspect -f '{{.State.Running}}' "$api_name" 2>/dev/null)" = true ] || break
    sleep 1
  done
  # The API never logs its URL or keys (Task 65), so its log is safe to show.
  docker logs "$api_name" 2>&1 | grep -E 'secrets key loaded|migrations applied|schema up to date|level=ERROR' | sed 's/^/  api: /' || true
  [ "$ready" = yes ] || stop "GoFlow didn't become ready (its log is above; MissingKeysError means the secrets key isn't the one that encrypted the webhook secrets)"
  mark "GoFlow ready on 127.0.0.1:$api_port"
  echo "Open http://127.0.0.1:$api_port, log in with your hosted account, and open the jobs list."
  read -rp "Press Enter once the restored jobs are on screen (that time ends the RTO): " _ || true
  mark "logged in, restored jobs visible"
  docker rm -f "$api_name" >/dev/null 2>&1 || true
  trap - EXIT
  echo "GoFlow stopped and its container removed."
fi
unset pw target target_nopw

# --- Times ----------------------------------------------------------------
end=$(date -u +%s)
echo
echo "==> times (UTC)"
printf '%s\n' "${steps[@]}"
printf '  %-40s %6ss\n' "this script, start to end" "$((end - start))"
if [ -n "$decided" ]; then
  d=$(date -u -d "$(date -u -d "@$end" +%Y-%m-%d) $decided" +%s)
  [ "$d" -gt "$end" ] && d=$((d - 86400))   # decided before midnight
  rto=$((end - d))
  printf '  %-40s %6ss   (%dm %02ds, from %s UTC)\n' "RTO: decision to end" "$rto" "$((rto / 60))" "$((rto % 60))" "$decided"
fi
echo "Write these into DEPLOYMENT.md (Disaster recovery, Results)."
