#!/usr/bin/env bash
# 初期データを生成し、isufactory と isufactory_init の両方に投入する。
# 使い方: load-data.sh <リポジトリの isufactory ディレクトリ> [mysql の追加オプション...]
set -euo pipefail
root="$1"; shift
mysqlc=(mysql --max-allowed-packet=256M "$@")
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

(cd "$root/tools/datagen" && go run . > "$tmp/initial.sql")

for db in isufactory isufactory_init; do
  "${mysqlc[@]}" -e "CREATE DATABASE IF NOT EXISTS $db"
  "${mysqlc[@]}" "$db" < "$root/webapp/sql/schema.sql"
  "${mysqlc[@]}" "$db" < "$tmp/initial.sql"
done
"${mysqlc[@]}" isufactory -e "INSERT INTO settings (name, value) VALUES ('plan_lock', '') ON DUPLICATE KEY UPDATE value = value"
