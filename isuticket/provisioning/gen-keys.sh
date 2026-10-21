#!/usr/bin/env bash
# QR 署名用の RSA-2048 鍵ペアを生成する。
set -euo pipefail
dir="${1:-/home/isucon/keys}"
mkdir -p "$dir"
if [ ! -f "$dir/qr_private.pem" ]; then
  openssl genrsa -traditional -out "$dir/qr_private.pem" 2048 2>/dev/null \
    || openssl genrsa -out "$dir/qr_private.pem" 2048
  openssl rsa -in "$dir/qr_private.pem" -pubout -out "$dir/qr_public.pem" 2>/dev/null
fi
chmod 600 "$dir/qr_private.pem"
chmod 644 "$dir/qr_public.pem"
