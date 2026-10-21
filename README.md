# isu-originals

ISUCON 形式のオリジナル問題のアプリケーション集。

| アプリ | 内容 |
|---|---|
| [ISUTICKET](isuticket/) | ライブチケット販売サービス |
| [ISUAUCTION](isuauction/) | 椅子専門のリアルタイムオークション |
| [ISUFACTORY](isufactory/) | 工場の生産ライン管理サービス |

## ディレクトリ構成

各アプリのディレクトリは同じ構成になっている。

```
webapp/go/        アプリケーション（Go）
webapp/public/    フロントエンド
webapp/sql/       スキーマと初期化 SQL
tools/datagen/    初期データ生成
bench/            ベンチマーカー
provisioning/     サーバ構築スクリプト・設定ファイル
dev/              ローカル用の Docker Compose
```
