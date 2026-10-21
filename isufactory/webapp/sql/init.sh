#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

mysql \
  -h "${ISUFACTORY_DB_HOST:-127.0.0.1}" \
  -P "${ISUFACTORY_DB_PORT:-3306}" \
  -u "${ISUFACTORY_DB_USER:-isucon}" \
  -p"${ISUFACTORY_DB_PASSWORD:-isucon}" \
  "${ISUFACTORY_DB_NAME:-isufactory}" < init.sql
