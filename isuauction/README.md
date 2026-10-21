# ISUAUCTION

椅子専門のリアルタイムオークションサービス。

## できること

- 出品中の商品を一覧・カテゴリ・キーワードで探す
- 商品の詳細で、現在価格と終了までの残り時間をリアルタイムに確認する
- 入札する（上限額を入れておくと、その金額まで自動で競り上げる）
- 気になる商品をウォッチする
- マイページで入札中・ウォッチ中・出品中の商品と、お知らせを確認する
- 椅子を出品する

終了間際に入札があると、終了時刻が少し延長される。

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
cd isuauction/dev
docker compose up -d mysql
docker compose --profile setup run --rm setup   # 初回のみ（初期データ投入、数分）
docker compose up -d app nginx
docker compose --profile bench run --rm bench   # ベンチマーク（約 2 分）
```

- ブラウザで <http://localhost:28080> を開く（例: `user00001` / `isu-user00001` でログイン）
- アプリを作り直したら `docker compose restart app nginx`

## サーバに構築する

Ubuntu 22.04 で、このリポジトリを clone してから root で実行する。

```bash
sudo isuauction/provisioning/setup-app.sh isuauction     # app1〜3
sudo isuauction/provisioning/setup-bench.sh isuauction   # bench
/home/isucon/bench/bench -target http://<app1 の IP>
```

ブラウザからは `/etc/hosts` で `isuauction.isucon.test` を app1 のパブリック IP に向けて
<https://isuauction.isucon.test/> を開く（自己署名証明書のため初回は警告が出る）。
