#!/usr/bin/env bash
# Checks a saved copy of GOFLOW_SECRETS_KEY without showing it (Task 81).
#
#   ./scripts/secrets-key-id.sh
#
# Paste the key from your password manager at the hidden prompt (paste,
# Enter, then Ctrl+D). It prints only the key's 8-character ID. Compare it
# with the line the hosted API logs at every start:
#   secrets key loaded key_id=...
# (Render: goflow-api, Logs, search "secrets key loaded"). Same ID: the
# copy is the key Render uses, so a restored database's webhook signing
# secrets can be read with it. Different ID: the copy is not that key.
#
# Nothing is written to disk and the key is never on a command line.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
. "$here/keyid.sh"
for c in base64 sha256sum od; do command -v "$c" >/dev/null || { echo "stop: $c is not installed" >&2; exit 1; }; done

printf 'GOFLOW_SECRETS_KEY from your password manager (paste, Enter, then Ctrl+D; nothing shows): ' >&2
if [ -t 0 ]; then stty -echo; trap 'stty echo' EXIT; fi
key=$(cat)
if [ -t 0 ]; then stty echo; trap - EXIT; fi
echo >&2
key=${key//[[:space:]]/}
if ! id=$(secrets_key_id "$key"); then
  unset key
  echo "stop: that copy can't be GoFlow's key; copy the whole value again" >&2
  exit 1
fi
unset key
echo "key_id=$id"
echo "Compare with Render's log line 'secrets key loaded key_id=...': they must be the same." >&2
