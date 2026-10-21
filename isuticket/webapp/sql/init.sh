#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

mysql \
  -h "${ISUTICKET_DB_HOST:-127.0.0.1}" \
  -P "${ISUTICKET_DB_PORT:-3306}" \
  -u "${ISUTICKET_DB_USER:-isucon}" \
  -p"${ISUTICKET_DB_PASSWORD:-isucon}" \
  "${ISUTICKET_DB_NAME:-isuticket}" < init.sql
