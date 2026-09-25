#!/usr/bin/env bash
# Opt-in live test; run inside a Linux container with bash, psql and coreutils.
set -euo pipefail
trap 'printf "FAIL: container renewal check at line %s\n" "$LINENO" >&2' ERR

if [[ "${1:-}" == --client ]]; then
  record=$2
  expected_user=$3
  expected_database=$4
  export PGOPTIONS='-c default_transaction_read_only=on'
  query() {
    psql -X -w -A -t -v ON_ERROR_STOP=1 -c \
      "SELECT pg_backend_pid(), current_user, current_database(), current_setting('transaction_read_only'), ssl FROM pg_stat_ssl WHERE pid=pg_backend_pid()"
  }

  first=$(query)
  IFS='|' read -r first_pid db_user db_name readonly tls <<< "$first"
  [[ "$db_user" == "$expected_user" && "$db_name" == "$expected_database" && "$readonly" == on && "$tls" == t ]]
  [[ "$(stat -c %a "$PGPASSFILE")" == 600 && "$(stat -c %a "$PGSERVICEFILE")" == 600 ]]
  [[ "$(stat -c %a "$(dirname "$PGPASSFILE")")" == 700 ]]
  # Reject password profiles without printing credential contents.
  grep -q 'X-Amz-Expires=900' "$PGPASSFILE"
  port=$(sed -n 's/^port=//p' "$PGSERVICEFILE")
  [[ "$port" =~ ^[0-9]+$ ]]
  printf '%s\n' "$PGPASSFILE" "$PGSERVICEFILE" "$port" > "$record"
  original=$(sha256sum "$PGPASSFILE")
  start=$SECONDS
  printf 'PASS: initial read-only TLS login and private file permissions; waiting 15m31s.\n'
  sleep 931
  [[ "$(sha256sum "$PGPASSFILE")" != "$original" ]]
  next=$(query)
  IFS='|' read -r next_pid db_user db_name readonly tls <<< "$next"
  [[ "$first_pid" != "$next_pid" && "$db_user" == "$expected_user" && "$db_name" == "$expected_database" && "$readonly" == on && "$tls" == t ]]
  printf 'PASS: refreshed credential file and fresh TLS backend after %ss.\n' "$((SECONDS-start))"
  exit 0
fi

if [[ $# != 4 ]]; then
  printf 'Usage: bash %s CONFIG CONNECTION EXPECTED_USER EXPECTED_DATABASE\n' "$0" >&2
  exit 2
fi

record_dir=$(mktemp -d)
trap 'rm -rf "$record_dir"' EXIT
psql --version
pg-tunnel --version
pg-tunnel run --config "$1" "$2" -- bash "$0" --client "$record_dir/session" "$3" "$4"
mapfile -t record < "$record_dir/session"
[[ ! -e "${record[0]}" && ! -e "${record[1]}" && ! -d "$(dirname "${record[0]}")" ]]
if (exec 3<>"/dev/tcp/127.0.0.1/${record[2]}") 2>/dev/null; then
  printf 'FAIL: tunnel listener survived cleanup\n' >&2
  exit 1
fi
printf 'PASS: private credential directory and tunnel listener removed.\n'
