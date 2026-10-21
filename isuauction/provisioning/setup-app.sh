#!/usr/bin/env bash
# app サーバ（app1〜3 共通）の構築スクリプト。Ubuntu 22.04 を root で実行する。
#
#   sudo ./setup-app.sh <isu-originals リポジトリの isuauction ディレクトリ>
#
# MySQL / nginx / アプリを導入する。
set -euo pipefail

SRC="$(cd "${1:?usage: setup-app.sh <isuauction dir>}" && pwd)"
GO_VERSION="${GO_VERSION:-1.23.4}"
HOME_DIR=/home/isucon

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends mysql-server nginx openssl ca-certificates curl git

if ! id isucon >/dev/null 2>&1; then
  useradd -m -s /bin/bash isucon
fi

# Go
if [ ! -x /usr/local/go/bin/go ] || ! /usr/local/go/bin/go version | grep -q "go${GO_VERSION} "; then
  arch="$(dpkg --print-architecture)"
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz" -o /tmp/go.tgz
  rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz && rm /tmp/go.tgz
fi
export PATH=/usr/local/go/bin:$PATH
grep -q /usr/local/go/bin "$HOME_DIR/.profile" || echo 'export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH' >> "$HOME_DIR/.profile"

# webapp
rsync -a --delete "$SRC/webapp/" "$HOME_DIR/webapp/" 2>/dev/null || { rm -rf "$HOME_DIR/webapp"; cp -r "$SRC/webapp" "$HOME_DIR/webapp"; }
mkdir -p "$HOME_DIR/tools" && cp -r "$SRC/tools/datagen" "$HOME_DIR/tools/"
[ -f "$HOME_DIR/env.sh" ] || cp "$SRC/provisioning/env.sh.example" "$HOME_DIR/env.sh"

chown -R isucon:isucon "$HOME_DIR"

# MySQL
cp "$SRC/provisioning/mysql/isuauction.cnf" /etc/mysql/mysql.conf.d/isuauction.cnf
systemctl restart mysql
mysql -uroot <<'SQL'
CREATE USER IF NOT EXISTS 'isucon'@'%' IDENTIFIED BY 'isucon';
CREATE DATABASE IF NOT EXISTS isuauction;
CREATE DATABASE IF NOT EXISTS isuauction_init;
GRANT ALL ON isuauction.* TO 'isucon'@'%';
GRANT ALL ON isuauction_init.* TO 'isucon'@'%';
SQL
if [ "$(mysql -uroot -N -e 'SELECT COUNT(*) FROM information_schema.tables WHERE table_schema="isuauction_init"')" = "0" ]; then
  # Ubuntu の MySQL root は auth_socket 認証なので root のまま実行する
  "$SRC/provisioning/load-data.sh" "$SRC" -uroot
fi

# アプリ
(cd "$HOME_DIR/webapp/go" && sudo -u isucon -H env PATH="$PATH" go build -o isuauction .)
cp "$SRC/provisioning/systemd/isuauction-go.service" /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now isuauction-go.service

# nginx（ブラウザ用の https は自己署名証明書）
"$SRC/provisioning/gen-tls.sh" "${ISUAUCTION_HOST:-isuauction.isucon.test}" /etc/nginx/tls
cp "$SRC/provisioning/nginx/isuauction.conf" /etc/nginx/sites-available/isuauction.conf
ln -sf /etc/nginx/sites-available/isuauction.conf /etc/nginx/sites-enabled/isuauction.conf
rm -f /etc/nginx/sites-enabled/default
nginx -t && systemctl restart nginx

echo "app setup done"
