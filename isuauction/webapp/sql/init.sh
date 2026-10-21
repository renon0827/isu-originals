#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

mysql \
  -h "${ISUAUCTION_DB_HOST:-127.0.0.1}" \
  -P "${ISUAUCTION_DB_PORT:-3306}" \
  -u "${ISUAUCTION_DB_USER:-isucon}" \
  -p"${ISUAUCTION_DB_PASSWORD:-isucon}" \
  "${ISUAUCTION_DB_NAME:-isuauction}" < init.sql
