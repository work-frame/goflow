# shellcheck shell=bash
# Sourced by secrets-key-id.sh and recover.sh (Task 81); not run on its own.
#
# secrets_key_id KEY prints the 8-character ID GoFlow gives the master key
# GOFLOW_SECRETS_KEY: the first 8 hex characters of the SHA-256 of its 32
# bytes, exactly as internal/secrets computes it. The API logs the same ID
# at startup ("secrets key loaded key_id=..."), so a saved copy of the key
# can be checked against the live one without ever showing either.
#
# It applies the API's own rules: standard base64 (44 characters, one "="),
# exactly 32 bytes, not one byte repeated. On a bad key it prints the
# reason to stderr, never the key, and returns 1. The key reaches base64
# and sha256sum through pipes (printf is a shell builtin), never as a
# command-line argument another process could read.
secrets_key_id() {
  local k="$1" n
  if ! [[ "$k" =~ ^[A-Za-z0-9+/]{43}=$ ]]; then
    echo "not a GoFlow secrets key: it must be 44 characters of standard base64 ending in one = (got ${#k} characters)" >&2
    return 1
  fi
  n=$(printf '%s' "$k" | base64 -d 2>/dev/null | wc -c)
  if [ "$n" != 32 ]; then
    echo "not a GoFlow secrets key: it decodes to $n bytes, want 32" >&2
    return 1
  fi
  if [ "$(printf '%s' "$k" | base64 -d | od -An -v -tx1 | tr -s ' \n' '\n' | grep . | sort -u | wc -l | tr -d ' ')" = 1 ]; then
    echo "not a GoFlow secrets key: it is one byte repeated" >&2
    return 1
  fi
  printf '%s' "$k" | base64 -d | sha256sum | cut -c1-8
}
