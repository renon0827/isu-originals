# ISUFACTORY

工場の生産ラインを管理するサービス。

## できること

**現場担当**
- ダッシュボードで、ラインと設備の稼働状況を確認する
- 設備ごとの計測値（温度・振動など）の推移と、作業予定・図面を確認する
- 計測値の異常から出たアラートを確認する
- 部材の入荷を登録し、在庫を確認する

**計画担当**
- 受注を登録する
- 受注をもとに生産計画を作り、設備ごとの作業予定（ガントチャート）を確認する

各設備の計測値は、設備側の送信端末から定期的に送られてくる。

## 構成

```
webapp/go/        アプリケーション（Go / echo / sqlx）
webapp/public/    フロントエンド
webapp/sql/       スキーマと初期化 SQL
tools/datagen/    初期データ生成
bench/            ベンチマーカー
provisioning/     サーバ構築スクリプト・設定ファイル
dev/              ローカル用の Docker Compose
```

## ローカルで動かす

Docker だけあれば動く。

```bash
cd isufactory/dev
docker compose up -d mysql
docker compose --profile setup run --rm setup   # 初回のみ（初期データ投入、1〜2 分）
docker compose up -d app nginx
docker compose --profile bench run --rm bench   # ベンチマーク（約 1 分半）
```

- ブラウザで <http://localhost:38080> を開く
  （現場担当 `operator01` / `isu-operator01`、計画担当 `planner01` / `isu-planner01`）
- アプリを作り直したら `docker compose restart app nginx`

## サーバに構築する

Ubuntu 22.04 で、このリポジトリを clone してから root で実行する。

```bash
sudo isufactory/provisioning/setup-app.sh isufactory     # app1〜3
sudo isufactory/provisioning/setup-bench.sh isufactory   # bench
/home/isucon/bench/bench -target http://<app1 の IP>
```

ブラウザからは `/etc/hosts` で `isufactory.isucon.test` を app1 のパブリック IP に向けて
<https://isufactory.isucon.test/> を開く（自己署名証明書のため初回は警告が出る）。
