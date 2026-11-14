#!/usr/bin/env bash
# Encrypted backup of GoFlow's hosted database (Phase 16, decision H2).
#
# Neon's Free plan keeps 6 hours of history: anything noticed later than
# that can't be restored from Neon. This makes a dump you keep yourself.
#
#   ./scripts/backup.sh               asks for the Neon URL (not shown)
#
# What it does, and nothing else:
#   - reads the database URL without echo (or from GOFLOW_BACKUP_DATABASE_URL)
#   - runs pg_dump (custom format) in the postgres:17 image, the same major
#     version as Neon, so this machine needs no PostgreSQL install
#   - pipes the dump straight into gpg --symmetric (AES-256): the dump is
#     never written to disk unencrypted. gpg asks for a passphrase twice
#   - writes ~/goflow-backups/goflow-<UTC time>.dump.gpg (folder mode 700,
#     file mode 600) and prints its size, SHA-256 and how long it took
# It never writes inside the repository and never prints the URL.
#
# The dump holds emails, password hashes, encrypted signing secrets (the
# master key is not in the database) and webhook URLs, which may carry
# tokens. Keep the passphrase in your password manager, never next to the
# file: without it the backup can't be read, by anyone, you included.
#
# GOFLOW_PG_TOOLS=local uses this machine's pg_dump instead of Docker (it
# must be version 17 or newer). GOFLOW_BACKUP_DIR changes the folder.
# GOFLOW_GPG_PASSFILE (tests only) reads the passphrase from a file.
set -euo pipefail

dir="${GOFLOW_BACKUP_DIR:-$HOME/goflow-backups}"
tools="${GOFLOW_PG_TOOLS:-docker}"
ca=/etc/ssl/certs/ca-certificates.crt

need() { command -v "$1" >/dev/null || { echo "stop: $1 is not installed" >&2; exit 1; }; }
need gpg
if [ "$tools" = docker ]; then need docker; else need pg_dump; fi

case "$dir" in
  "$(git rev-parse --show-toplevel 2>/dev/null || echo /nonexistent)"*)
    echo "stop: $dir is inside the repository; backups never go there" >&2; exit 1 ;;
esac

url="${GOFLOW_BACKUP_DATABASE_URL:-}"
if [ -z "$url" ]; then
  read -rsp "Neon database URL (not shown): " url
  echo
fi
case "$url" in
  postgres://*|postgresql://*) ;;
  *) echo "stop: that isn't a postgres:// URL" >&2; exit 1 ;;
esac

umask 077
mkdir -p "$dir"
chmod 700 "$dir"
out="$dir/goflow-$(date -u +%Y%m%dT%H%M%SZ).dump.gpg"
tmp="$out.partial"
trap 'rm -f "$tmp"' EXIT

gpg_args=(--symmetric --cipher-algo AES256 --output "$tmp")
if [ -n "${GOFLOW_GPG_PASSFILE:-}" ]; then
  gpg_args=(--batch --pinentry-mode loopback --passphrase-file "$GOFLOW_GPG_PASSFILE" "${gpg_args[@]}")
fi

start=$(date +%s)
echo "==> dumping and encrypting (gpg will ask for a passphrase twice)"
if [ "$tools" = docker ]; then
  # libpq checks the server's certificate against this machine's CA
  # bundle, mounted read-only: verify-full works in any image.
  sep='?'; case "$url" in *\?*) sep='&' ;; esac
  PGURL="${url}${sep}sslrootcert=/etc/goflow-ca.crt" \
    docker run --rm -i -e PGURL -v "$ca:/etc/goflow-ca.crt:ro" postgres:17 \
      sh -c 'exec pg_dump --format=custom --no-owner --no-privileges --dbname="$PGURL"' </dev/null |
    gpg "${gpg_args[@]}"
else
  pg_dump --format=custom --no-owner --no-privileges --dbname="$url" </dev/null | gpg "${gpg_args[@]}"
fi
unset url
mv "$tmp" "$out"
trap - EXIT
took=$(( $(date +%s) - start ))

echo "==> backup written"
echo "file:    $out"
echo "size:    $(stat -c %s "$out") bytes"
echo "sha256:  $(sha256sum "$out" | cut -c1-64)"
echo "took:    ${took}s"
echo "Next: prove it restores (./scripts/restore-drill.sh \"$out\")."
