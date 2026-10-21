#!/usr/bin/env bash
# ブラウザ用の自己署名 TLS 証明書を生成する。
set -euo pipefail
host="${1:-isufactory.isucon.test}"
dir="${2:-/etc/nginx/tls}"
mkdir -p "$dir"
if [ ! -f "$dir/isufactory.crt" ]; then
  openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
    -subj "/CN=$host" -addext "subjectAltName=DNS:$host" \
    -keyout "$dir/isufactory.key" -out "$dir/isufactory.crt" 2>/dev/null
fi
chmod 600 "$dir/isufactory.key"
