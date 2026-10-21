#!/usr/bin/env bash
# bench サーバの構築スクリプト。Ubuntu 22.04 を root で実行する。
#
#   sudo ./setup-bench.sh <isu-originals リポジトリの isuauction ディレクトリ>
#
# 実行例:
#   /home/isucon/bench/bench -target http://<app1 の private IP>
set -euo pipefail

SRC="$(cd "${1:?usage: setup-bench.sh <isuauction dir>}" && pwd)"
GO_VERSION="${GO_VERSION:-1.23.4}"

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends ca-certificates curl git

if ! id isucon >/dev/null 2>&1; then
  useradd -m -s /bin/bash isucon
fi
if [ ! -x /usr/local/go/bin/go ] || ! /usr/local/go/bin/go version | grep -q "go${GO_VERSION} "; then
  arch="$(dpkg --print-architecture)"
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz" -o /tmp/go.tgz
  rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz && rm /tmp/go.tgz
fi
export PATH=/usr/local/go/bin:$PATH

mkdir -p /home/isucon/bench
(cd "$SRC/bench" && go build -o /home/isucon/bench/bench .)
chown -R isucon:isucon /home/isucon/bench

# fd 上限を上げる
cat > /etc/security/limits.d/isucon.conf <<'LIMITS'
isucon soft nofile 65535
isucon hard nofile 65535
LIMITS

echo "bench setup done"
