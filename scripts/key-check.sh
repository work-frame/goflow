#!/usr/bin/env bash
# The live API key check (Task 88): seven steps against the hosted API
# with a real key, then the key is revoked.
#
#   ./scripts/key-check.sh [BASE_URL]
#
# BASE_URL defaults to https://goflow-api-hewh.onrender.com. Before you
# run it, create a key in the dashboard (API keys, Create key):
#   name "live test", scope jobs:create only, 30 days
# and keep the dialog open until you have pasted the key here.
#
# It asks, all hidden: the account email, the account password, the key.
# Then:
#   1. the key is in the account's list: active, named "live test", same hint
#   2. the key creates a job: 201, and the job names the key (api_key_id)
#   3. the session reads that job: 200, same account, same api_key_id
#   4. the key lists jobs: 403, naming the missing jobs:read scope
#   5. the key calls a session-only route (/auth/me): 403
#   6. the session revokes the key: 204, and the list says revoked
#   7. the key creates a job again: 401, at once
# Any other status or field stops it with FAIL and exit 1.
#
# Whatever happens after step 1, the key is revoked before it exits
# (Ctrl+C included), and the session it opened is logged out. If even
# that fails it says so: then revoke the key in the dashboard.
#
# Secrets: the key, the password and the session token are never on a
# command line, never printed and never written to a file. curl gets the
# Authorization header and the login body through /dev/fd (process
# substitution), not through its arguments. It refuses to run with shell
# tracing on (bash -x), which would print them. Output names the key by
# its 4-character hint and IDs by their first 8 characters.
#
# The job from step 2 stays in the account, so the dashboard shows it
# with "Created by live test". Delete it there when you like.
#
# Tests only: BASE_URL may be http://127.0.0.1:PORT or http://localhost:PORT.
if [[ $- == *x* ]]; then
  echo "stop: shell tracing is on (bash -x); it would print the key. Run ./scripts/key-check.sh" >&2
  exit 2
fi
set -euo pipefail

base="${1:-https://goflow-api-hewh.onrender.com}"
base="${base%/}"
case "$base" in
  https://*) ;;
  http://127.0.0.1:* | http://localhost:*) ;;
  *) echo "usage: $0 [https://...]  (http only for 127.0.0.1 or localhost)" >&2; exit 2 ;;
esac
for c in curl jq od; do command -v "$c" >/dev/null || { echo "stop: $c is not installed" >&2; exit 1; }; done

# call METHOD PATH TOKEN [BODY] sets status and body. TOKEN "" sends no
# Authorization header. curl reads the header (a config file) and the
# body from /dev/fd, so neither is in its arguments.
status="" body="" idem=""
auth_config() { [ -z "$1" ] || printf 'header = "Authorization: Bearer %s"\n' "$1"; }
call() {
  local args=(-sS --max-time 30 -X "$1" -w '\n%{http_code}') out
  [ -z "$idem" ] || args+=(-H "Idempotency-Key: $idem")
  if [ -n "${4:-}" ]; then
    out=$(curl "${args[@]}" -K <(auth_config "$3") -H 'Content-Type: application/json' --data-binary @<(printf '%s' "$4") "$base$2" 2>/dev/null) || out=""
  else
    out=$(curl "${args[@]}" -K <(auth_config "$3") "$base$2" 2>/dev/null) || out=""
  fi
  if [ -z "$out" ]; then
    status=000 body=""
    return 0
  fi
  status="${out##*$'\n'}"
  body="${out%$'\n'*}"
}
field() { printf '%s' "$body" | jq -r "$1" 2>/dev/null || true; }
errmsg() { local e; e=$(field '.error // empty'); printf '%s' "${e:-no error message}"; }

passed=0
pass() { passed=$((passed + 1)); printf 'PASS %d/7 %s\n' "$1" "$2"; }
fail() { printf 'FAIL %d/7 %s\n' "$1" "$2"; exit 1; }

email="" pw="" key=""
read -rsp 'Account email (hidden): ' email || true; [ ! -t 0 ] || echo >&2
read -rsp 'Account password (hidden): ' pw || true; [ ! -t 0 ] || echo >&2
read -rsp 'API key from the dashboard dialog (hidden): ' key || true; [ ! -t 0 ] || echo >&2
key="${key//[[:space:]]/}"
if ! [[ "$key" =~ ^gfk_[A-Za-z0-9_-]{43}$ ]]; then
  unset key pw
  echo "stop: that isn't a whole gfk_ key; copy it again from the dialog" >&2
  exit 1
fi
hint="${key: -4}"

session="" key_id="" revoked=0
cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  idem=""
  if [ -n "$key_id" ] && [ "$revoked" = 0 ]; then
    call DELETE "/api/v1/keys/$key_id" "$session"
    if [ "$status" = 204 ]; then
      echo "cleanup: key ...$hint revoked"
    else
      echo "CLEANUP FAILED: key ...$hint may still be active (status $status). Revoke it in the dashboard now." >&2
      rc=1
    fi
  elif [ -z "$key_id" ]; then
    echo "note: nothing was revoked. If you won't run this again, revoke \"live test\" in the dashboard." >&2
  fi
  if [ -n "$session" ]; then
    call POST /api/v1/auth/logout "$session"
    [ "$status" = 204 ] || echo "note: logout answered $status; the session expires on its own" >&2
  fi
  unset key pw session email
  [ "$rc" = 0 ] && echo "all 7 steps passed against $base"
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

echo "waking $base (a free instance can take about 50 s)..."
for i in $(seq 1 18); do
  call GET /healthz ""
  [ "$status" = 200 ] && break
  [ "$i" = 18 ] && { echo "stop: /healthz didn't answer 200 within 90 s (last: $status)" >&2; exit 1; }
  sleep 5
done

login=$(printf '%s\n%s' "$email" "$pw" | jq -Rs 'split("\n") | {email: .[0], password: .[1]}')
unset pw
call POST /api/v1/auth/login "" "$login"
unset login
[ "$status" = 200 ] || { echo "stop: login answered $status: $(errmsg)" >&2; exit 1; }
session=$(field '.token // empty')
[ -n "$session" ] || { echo "stop: login answered 200 without a token" >&2; exit 1; }

# 1. Find the key in this account's list. Nothing else is revoked: a key
# that isn't exactly one active "live test" with this hint stops here.
call GET /api/v1/keys "$session"
[ "$status" = 200 ] || fail 1 "list keys: status $status, want 200: $(errmsg)"
mine='[.keys[] | select(.name == "live test" and .status == "active" and .hint == $h)]'
matches=$(printf '%s' "$body" | jq -r --arg h "$hint" "$mine | length" 2>/dev/null) || matches="none (the list isn't JSON)"
if [ "$matches" != 1 ]; then
  fail 1 "want exactly one active key named \"live test\" ending in $hint in this account, found $matches"
fi
key_id=$(printf '%s' "$body" | jq -r --arg h "$hint" "$mine | .[0].id")
scopes=$(printf '%s' "$body" | jq -r --arg h "$hint" "$mine | .[0].scopes | join(\",\")")
pass 1 "key ...$hint is active in this account (id ${key_id:0:8}, scopes $scopes)"

# 2. The key creates a job, and the job names the key.
idem="key-check-$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
call POST /api/v1/jobs "$key" '{"title":"live test (Task 88)"}'
idem=""
[ "$status" = 201 ] || fail 2 "create a job with the key: status $status, want 201: $(errmsg)"
job_id=$(field '.id // empty')
[ -n "$job_id" ] || fail 2 "create a job with the key: 201 without an id"
[ "$(field '.api_key_id // empty')" = "$key_id" ] || fail 2 "job ${job_id:0:8}: api_key_id is \"$(field '.api_key_id // empty')\", want ${key_id:0:8}"
pass 2 "the key created job ${job_id:0:8} (201, status $(field .status), api_key_id ${key_id:0:8})"

# 3. The session (the account) reads the job: it is this account's, made by this key.
call GET "/api/v1/jobs/$job_id" "$session"
[ "$status" = 200 ] || fail 3 "read job ${job_id:0:8} with the session: status $status, want 200: $(errmsg)"
[ "$(field .id)" = "$job_id" ] || fail 3 "read job ${job_id:0:8}: got id $(field .id)"
[ "$(field '.api_key_id // empty')" = "$key_id" ] || fail 3 "job ${job_id:0:8} as the account sees it: api_key_id \"$(field '.api_key_id // empty')\", want ${key_id:0:8}"
pass 3 "the account owns job ${job_id:0:8} and it is attributed to key ${key_id:0:8} (200)"

# 4. Without jobs:read, the key can't list jobs.
call GET /api/v1/jobs "$key"
[ "$status" = 403 ] || fail 4 "list jobs with the key (scopes $scopes): status $status, want 403"
[[ "$(errmsg)" == *"jobs:read"* ]] || fail 4 "list jobs: the 403 doesn't name jobs:read: $(errmsg)"
pass 4 "the key can't list jobs (403: $(errmsg))"

# 5. A key can't use a session-only route.
call GET /api/v1/auth/me "$key"
[ "$status" = 403 ] || fail 5 "/auth/me with the key: status $status, want 403"
[ "$(errmsg)" = "API keys can't use this route; log in instead" ] || fail 5 "/auth/me with the key: $(errmsg)"
pass 5 "the key can't use /auth/me (403: $(errmsg))"

# 6. Revoke it, and the list says so.
call DELETE "/api/v1/keys/$key_id" "$session"
[ "$status" = 204 ] || fail 6 "revoke key ${key_id:0:8}: status $status, want 204: $(errmsg)"
revoked=1
call GET /api/v1/keys "$session"
[ "$status" = 200 ] || fail 6 "list keys after revoking: status $status, want 200"
st=$(printf '%s' "$body" | jq -r --arg id "$key_id" '.keys[] | select(.id == $id) | .status')
used=$(printf '%s' "$body" | jq -r --arg id "$key_id" '.keys[] | select(.id == $id) | .last_used_at // "never"')
[ "$st" = revoked ] || fail 6 "key ${key_id:0:8} is \"$st\" after revoking, want revoked"
pass 6 "key ${key_id:0:8} revoked (204; list says revoked, last used $used)"

# 7. The revoked key is refused at once: no cache.
call POST /api/v1/jobs "$key" '{"title":"must not exist"}'
[ "$status" = 401 ] || fail 7 "create a job with the revoked key: status $status, want 401"
pass 7 "the revoked key is refused (401: $(errmsg))"
