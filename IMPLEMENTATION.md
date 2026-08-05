# 実装状況と次のステップ

## 現在地（2026-08-02 時点）
- SAM プロジェクト初期化済み（Go / provided.al2023 / arm64）。
- Git 初期化・リモート登録・初回コミット済み。`.gitignore` 設定済み（`.aws-sam/` 等を除外）。
- DynamoDB Local を Docker で起動し、`scripts/create-table.sh` でメインテーブル `kotozute`（PK/SK）を作成済み（ダミー認証情報で統一。理由は下記「ローカル開発」参照）。
- **`POST /kotozute`（ことづてを書く）が完成・動作確認済み。**
  - `hello-world/` を `functions/post-kotozute/` に整理（Hello World は削除、最初の実関数に差し替え）。
  - AWS SDK for Go v2（`config` / `dynamodb`）と `google/uuid` を依存に追加。
  - `pairID=p01` / `userID=k01` の固定値で `PutItem`。`SK` は `KOTOZUTE#<userID>#<ISO日時>#<uuid>`、`opened=false` で作成。
  - `sam local invoke` と `sam local start-api` の両方で、DynamoDB Local（`http://host.docker.internal:8000`）への書き込みを確認済み。バリデーションエラー（空 body / 不正 JSON）が 400 を返すことも確認済み。
- **`GET /kotozute/this-week`（相互ロックの中心）が完成・動作確認済み。**
  - `functions/get-this-week/` を新規作成（`post-kotozute`と同じ構成：独立した`go.mod`、`init()`で共有DynamoDBクライアント）。
  - 相手の固定ユーザーIDとして`k02`を新設（`k01`と対。認証・ペア機能導入まわりの仮値）。
  - `PAIR#p01/META`が未整備でも動くよう、`GetItem`結果が空ならコード内のデフォルト値（`lastOpenedAt`は遠い過去のsentinel、`openDayOfWeek`=5(金)、`openTime`=20:00、`timezone`=Asia/Tokyo）にフォールバックする設計。実データが入れば自動的にそちらへ切り替わる。
  - DynamoDBクエリは`SK BETWEEN "KOTOZUTE#<userId>#<lastOpenedAt>" AND "KOTOZUTE#<userId>#￿"`のBETWEENトリックで、begins_withと範囲条件を同時に満たす。自分・相手それぞれ1回、計2回クエリしGo側で相互ロック判定。
  - `openStatus`・`nextOpenAt`もCLAUDE.mdのレスポンス契約通りフル実装（`nextOpenAt`はPATCH /pair・バッチ未実装の現段階では近似値）。
  - `canSeePartner=false`のときは`partnerKotozute`に一切触れない（詰めるコードパス自体に到達しない）設計で本文漏洩を構造的に回避。ユニットテストで本文が漏れないことを直接assert。
  - CLAUDE.mdのロック表4パターン＋境界値（`lastOpenedAt`より前のアイテムが範囲外になること）を`sam local invoke`でDynamoDB Localに対して実地確認済み。

## プロジェクト構成（現状）
```
kotozute/
├── template.yaml               # SAM。PostKotozuteFunction(POST /kotozute) を定義
├── functions/
│   ├── post-kotozute/
│   │   ├── main.go             # POST /kotozute ハンドラ
│   │   ├── main_test.go
│   │   └── go.mod / go.sum
│   └── get-this-week/
│       ├── main.go             # GET /kotozute/this-week ハンドラ
│       ├── main_test.go
│       └── go.mod / go.sum
├── events/
│   ├── event.json              # (旧Hello World用、参考)
│   ├── post-kotozute.json      # sam local invoke 用サンプルイベント
│   └── get-this-week.json      # sam local invoke 用サンプルイベント
├── scripts/
│   ├── create-table.sh         # DynamoDB Local にメインテーブルを作成（ダミーキー）
│   └── db-admin.sh             # dynamodb-admin（GUI）をダミーキーで起動
├── env.json                    # ローカル用 DYNAMODB_ENDPOINT・ダミー認証情報（gitignore済み。各自作成する）
├── Makefile
├── CLAUDE.md                   # プロジェクト指示書（先に読む）
└── IMPLEMENTATION.md            # このファイル
```

### ローカル開発の認証情報（ダミーキーに統一）
DynamoDB Local は「リージョン × アクセスキー」ごとにデータ領域が分かれる。本番の IAM キーをローカル接続に使うと、キーをローテーションしたときにローカルのテーブルが迷子になる。そのため、ローカルで DynamoDB Local を触るときは CLI でも Lambda ローカル実行でも必ず以下のダミー認証情報で統一する（本番キーはローカルには一切使わない）。

```
AWS_ACCESS_KEY_ID=local
AWS_SECRET_ACCESS_KEY=local
AWS_REGION=ap-northeast-1
```

CLI からの操作は `scripts/` 配下のスクリプトを使う（両方ダミーキー・実行権限付与済み）:
```bash
./scripts/create-table.sh   # メインテーブル kotozute（PK/SK）を作成
./scripts/db-admin.sh       # dynamodb-admin（GUI）を起動
```
※ GSI①②は未定義。GSI を使う機能（ログイン、バッチ）を実装するときは `scripts/create-table.sh` に GSI 定義を追加する。

### ローカルで POST /kotozute を試す
`sam local` は Lambda を Docker コンテナ内で動かすため、コンテナから見たローカル DB のエンドポイントは `http://localhost:8000` ではなく `http://host.docker.internal:8000` になる。Go コードは環境変数 `DYNAMODB_ENDPOINT` で接続先を切り替える設計なので、ローカル実行時はこれを `http://host.docker.internal:8000` に、認証情報を上記ダミーキーにする。

```bash
# 1回だけの呼び出し確認
sam build
sam local invoke PostKotozuteFunction --event events/post-kotozute.json --env-vars env.json
sam local invoke GetThisWeekFunction --event events/get-this-week.json --env-vars env.json

# HTTPサーバとして立てて確認
sam local start-api --env-vars env.json
curl -X POST http://127.0.0.1:3000/kotozute -H 'Content-Type: application/json' -d '{"body":"ありがとう"}'
curl http://127.0.0.1:3000/kotozute/this-week
```
`env.json`（gitignore対象。各自ローカルに作成。関数が増えたらキーを追加する）:
```json
{
  "PostKotozuteFunction": {
    "DYNAMODB_ENDPOINT": "http://host.docker.internal:8000",
    "AWS_ACCESS_KEY_ID": "local",
    "AWS_SECRET_ACCESS_KEY": "local",
    "AWS_REGION": "ap-northeast-1"
  },
  "GetThisWeekFunction": {
    "DYNAMODB_ENDPOINT": "http://host.docker.internal:8000",
    "AWS_ACCESS_KEY_ID": "local",
    "AWS_SECRET_ACCESS_KEY": "local",
    "AWS_REGION": "ap-northeast-1"
  }
}
```
`GET /kotozute/this-week`の相互ロックをローカルで確認する場合は、`aws dynamodb put-item`（ダミー認証情報つき）で`PAIR#p01`配下に`k01`/`k02`のことづてアイテムや`PAIR#p01/META`を投入してシナリオを作る（自分の枚数・相手の`opened`フラグの組み合わせを変えて`myKotozute`/`canSeePartner`/`partnerHasWritten`の出方を確認）。

## 次にやること（順序）

### 1. ユーザー／ペア／招待まわり
- `POST /users`, `GET /me`, 招待フロー（`POST /invites` → accept でペア成立、lastOpenedAt 初期化）。
- これが実装されると `PAIR#p01/META` に実データが入り、`get-this-week` のデフォルト値フォールバックが自然に実データ経路へ切り替わる。相手の固定ID（`k02`）や`pairID`/`userID`の固定値も、この時点で置き換え対象になる。

### 2. 認証（Cognito）を被せる
- Cognito ユーザープール作成（メール＋パスワード標準フロー）。
- API Gateway に Cognito Authorizer。ハンドラ内で固定 userID を「トークンから取得」に差し替え。

### 3. 週一オープンのバッチ
- GSI① を作成（テーブル作成コマンドに GSI 定義を追加）。
- EventBridge Scheduler → Lambda。オープン処理＋通知の出し分け＋lastOpenedAt 更新。両者ゼロはスキップ。

### 4. 通知（SNS→APNs）／アーカイブのページング
- APNs 設定、SNS 連携。`GET /kotozute/archive` のカーソルページング。

## 設計の出典
全仕様は Notion「ことづて」設計ドキュメント（開発ページ配下）にある。矛盾があれば Notion を正とする。主要点は CLAUDE.md に転記済み。

## 注意点（過去に決めた重要な判断）
- フレームワーク（Gin等）は使わない。素の aws-lambda-go。
- 相互ロックはサーバー側判定・本文を返さない（セキュリティ）。
- 削除は実装しない。編集はオープン前・本文のみ。
- 気配は「存在のみ」、枚数は出さない、相手0枚は無言。
- 「今週」はカレンダー週でなく lastOpenedAt 境界。
