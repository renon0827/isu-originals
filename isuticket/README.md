# ISUTICKET

人気アーティストのライブチケットを販売するサービス。

## できること

**ファン**
- 公演の一覧・詳細・座席表を見る
- 先着販売の公演で座席を選んで購入する（1 回 4 席まで）
- 抽選販売の公演に申し込み、発表後に結果を確認する
- マイチケットで購入したチケットと入場用の QR コードを確認する
- 行けなくなったチケットを公式リセールに出品する、出品されたチケットを購入する

**主催者**
- 公演を登録する（会場・販売方式・販売期間・価格）
- 公演ごとの売上を確認する
- 入場ゲートで QR コードを読み取り、入場を記録する

チケットの購入代金は外部の決済サービスで決済する。

## 構成

```
webapp/go/        アプリケーション（Go / echo / sqlx）
webapp/public/    フロントエンド
webapp/sql/       スキーマと初期化 SQL
tools/datagen/    初期データ生成
bench/            ベンチマーカー（決済サービスのモックを含む）
provisioning/     サーバ構築スクリプト・設定ファイル
dev/              ローカル用の Docker Compose
```

## ローカルで動かす

Docker だけあれば動く。

```bash
cd isuticket/dev
docker compose up -d mysql
docker compose --profile setup run --rm setup   # 初回のみ（初期データ投入、数分）
docker compose up -d app nginx
docker compose --profile bench run --rm bench   # ベンチマーク（約 2 分）
```

- ブラウザで <http://localhost:18080> を開く（例: `fan00001` / `isu-fan00001` でログイン）
- アプリを作り直したら `docker compose restart app nginx`

## サーバに構築する

Ubuntu 22.04 で、このリポジトリを clone してから root で実行する。

```bash
sudo isuticket/provisioning/setup-app.sh isuticket     # app1〜3
sudo isuticket/provisioning/setup-bench.sh isuticket   # bench
/home/isucon/bench/bench -target http://<app1 の IP> -payment-url http://<bench の IP>:12346
```

ブラウザからは `/etc/hosts` で `isuticket.isucon.test` を app1 のパブリック IP に向けて
<https://isuticket.isucon.test/> を開く（自己署名証明書のため初回は警告が出る）。

QR コードの署名鍵はサーバの `/home/isucon/keys/` に置かれる。
