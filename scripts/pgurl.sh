# shellcheck shell=bash
# Sourced by backup.sh and count-rows.sh (Task 69b); not run on its own.
#
# verified_url URL CAFILE prints URL with sslmode=verify-full and
# sslrootcert=CAFILE, replacing any sslmode or sslrootcert already in it.
# Neon's connection string says sslmode=require, which encrypts but checks
# nothing; with a root certificate added, libpq would then check the
# certificate's issuer but not its name (verify-ca), so any certificate
# from a public CA for any host would pass. verify-full checks both.
#
# A URL for localhost, 127.0.0.1 or ::1 is printed unchanged: that
# connection never leaves this machine (the tests' local PostgreSQL has no
# TLS). The output is only ever used inside the script, never printed.
verified_url() {
  local url="$1" ca="$2" base query host
  case "$url" in
    *\?*) base="${url%%\?*}" query="${url#*\?}" ;;
    *) base="$url" query="" ;;
  esac
  host="${base#*://}"   # user:pass@host:port/db
  host="${host%%/*}"    # user:pass@host:port
  host="${host##*@}"    # host:port or [::1]:port
  case "$host" in
    localhost|localhost:*|127.0.0.1|127.0.0.1:*|\[::1\]|\[::1\]:*)
      printf '%s\n' "$url"; return 0 ;;
  esac
  local kept=() parts=() part
  if [ -n "$query" ]; then
    IFS='&' read -ra parts <<<"$query"
    for part in "${parts[@]}"; do
      case "$part" in
        sslmode=*|sslrootcert=*|"") ;;
        *) kept+=("$part") ;;
      esac
    done
  fi
  query="$(IFS='&'; printf '%s' "${kept[*]-}")"
  printf '%s?%ssslmode=verify-full&sslrootcert=%s\n' "$base" "${query:+$query&}" "$ca"
}
