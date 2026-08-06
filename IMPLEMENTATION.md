# 実装状況と次のステップ

## 現在地（2026-08-06 時点）
- SAM プロジェクト初期化済み（Go / provided.al2023 / arm64）。
- Git 初期化・リモート登録・初回コミット済み。`.gitignore` 設定済み（`.aws-sam/` 等を除外）。
- DynamoDB Local を Docker で起動し、`scripts/create-table.sh` でメインテーブル `kotozute`（PK/SK）を作成済み。
- **`POST /kotozute`（ことづてを書く）が完成・動作確認済み。**
  - `hello-world/` を `functions/post-kotozute/` に整理（Hello World は削除、最初の実関数に差し替え）。
  - AWS SDK for Go v2（`config` / `dynamodb`）と `google/uuid` を依存に追加。
  - `pairID=p01` / `userID=k01` の固定値で `PutItem`。`SK` は `KOTOZUTE#<userID>#<ISO日時>#<uuid>`、`opened=false` で作成。
  - `sam local invoke` と `sam local start-api` の両方で、DynamoDB Local への書き込みを確認済み。バリデーションエラー（空 body / 不正 JSON）が 400 を返すことも確認済み。
- **`GET /kotozute/this-week`（相互ロックの中心）が完成・動作確認済み。**
  - `functions/get-this-week/` を新規作成（`post-kotozute`と同じ構成：独立した`go.mod`、`init()`で共有DynamoDBクライアント）。
  - 相手の固定ユーザーIDとして`k02`を新設（`k01`と対。認証・ペア機能導入まわりの仮値）。
  - `PAIR#p01/META`が未整備でも動くよう、`GetItem`結果が空ならコード内のデフォルト値（`lastOpenedAt`は遠い過去のsentinel、`openDayOfWeek`=5(金)、`openTime`=20:00、`timezone`=Asia/Tokyo）にフォールバックする設計。実データが入れば自動的にそちらへ切り替わる。
  - DynamoDBクエリは`SK BETWEEN "KOTOZUTE#<userId>#<lastOpenedAt>" AND "KOTOZUTE#<userId>#￿"`のBETWEENトリックで、begins_withと範囲条件を同時に満たす。自分・相手それぞれ1回、計2回クエリしGo側で相互ロック判定。
  - `openStatus`・`nextOpenAt`もCLAUDE.mdのレスポンス契約通りフル実装（`nextOpenAt`はPATCH /pair・バッチ未実装の現段階では近似値）。
  - `canSeePartner=false`のときは`partnerKotozute`に一切触れない（詰めるコードパス自体に到達しない）設計で本文漏洩を構造的に回避。ユニットテストで本文が漏れないことを直接assert。
- **ユーザー／ペア／招待まわり（`POST /users`, `GET /me`, `POST /invites`, `GET /invites/{token}`, `POST /invites/{token}/accept`）が完成・動作確認済み。**
  - 5関数を新規作成（`functions/post-users/`, `get-me/`, `post-invites/`, `get-invite/`, `post-invite-accept/`）。いずれも既存関数と同じ構成（独立go.mod、`init()`で共有DynamoDBクライアント、`errorResponse`ヘルパー）。
  - `POST /users`は「登録するユーザー」を`k01`固定にし、`PutItem`に`ConditionExpression: attribute_not_exists(PK)`をつけて二重登録を409で防ぐ。
  - `POST /invites`はUUIDトークンを発行し`INVITE#<token>/META`に`inviterUserId`/`status=pending`/`ttl`（7日後のunix epoch秒）を保存。未登録・既にペア済みなら400/409。
  - `GET /invites/{token}`はttl期限切れを404として扱い（存在有無を漏らさない）、招待者の`displayName`を返す。
  - `POST /invites/{token}/accept`は招待を受ける側を`k02`固定にし、`TransactWriteItems`で「INVITE状態をaccepted に更新（pending かつ未期限切れが条件）」「PAIR#p01/METAを新規作成（既存なら失敗）」「招待者のUSER METAにpairIdをセット（既にペア済みなら失敗）」「受諾者のUSER METAをpairId付きでPut（既存のemail/apnsTokenは引き継ぐ）」の4件を原子的に実行し、ペア成立を保証する。
  - **バグ修正の記録**：`ttl`はDynamoDBの予約語のため、ConditionExpression内で直接使うと`ValidationException`になる（`status`と同様に`#ttl`エイリアスが必要）。実装時に`sam local invoke`経由の500エラーから発見し、`aws dynamodb transact-write-items`で同じリクエストを直接再現して原因を特定した。
  - 動作確認：登録→招待発行→招待閲覧→受諾→`GET /me`でペア成立確認→`GET /kotozute/this-week`が実データの`PAIR#p01/META`を自然に使うようになることまで一気通貫で確認済み。二重登録・二重受諾・期限切れ・既にペア済みでの招待発行がそれぞれ409/404で弾かれることも確認済み。

### 重要な発見：`sam local invoke`/`start-api`はAWS認証情報系の環境変数を`--env-vars`で上書きできない
`DYNAMODB_ENDPOINT`のような独自の環境変数は`env.json`の指定どおりコンテナに渡るが、`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`/`AWS_REGION`は「Lambda予約環境変数」としてSAM CLIが実行ホストの `~/.aws/credentials` の `default` プロファイルから自動的に注入し、`env.json`の値やコマンドラインの`--profile`指定を上書きする（実機のLambda実行環境がIAMロールの認証情報を自動注入し、ユーザーが上書きできないのと同じ挙動をローカルでも再現している）。
- そのため、ホストマシンに実際のIAM認証情報（`~/.aws/credentials`の`[default]`）が設定されていると、`sam local invoke`から書き込まれるデータはそのIAMキー×リージョンのDynamoDB Local領域に入る。`env.json`に書いた`AWS_ACCESS_KEY_ID=local`は効かない。
- DynamoDB Localへの接続先自体（エンドポイント）は正しく`DYNAMODB_ENDPOINT`で制御できているので、本物のAWSに繋がってしまうわけではない（実害はない）。ただし「ダミーキーに統一」という当初の意図（IAMキーローテーション時にローカルデータが迷子にならないようにする）は、`sam local invoke`/`start-api`経由の書き込みに関しては実現できていない。
- `scripts/create-table.sh` / `scripts/db-admin.sh` はSAMを経由しない直接のCLI呼び出しなので、ここでのダミーキー統一は引き続き有効。
- 対応：この制約を認識した上で、`sam local invoke`でのテスト時はホストの`default`プロファイル（現状のIAMキー）がDynamoDB Localの実データ領域になることを前提にする。動作確認時にCLIでデータを直接見る/投入する場合も、ダミーキーではなくデフォルトプロファイル（環境変数を明示的にunset/exportしない状態）を使うこと。

## プロジェクト構成（現状）
```
kotozute/
├── template.yaml               # SAM。8関数を定義
├── functions/
│   ├── post-kotozute/          # POST /kotozute
│   ├── get-this-week/          # GET /kotozute/this-week
│   ├── post-users/             # POST /users
│   ├── get-me/                 # GET /me
│   ├── post-invites/           # POST /invites
│   ├── get-invite/             # GET /invites/{token}
│   └── post-invite-accept/     # POST /invites/{token}/accept
│       （各ディレクトリに main.go / main_test.go / go.mod / go.sum）
├── events/                     # sam local invoke 用サンプルイベント（関数ごと）
├── scripts/
│   ├── create-table.sh         # DynamoDB Local にメインテーブルを作成（ダミーキー、CLI直接用）
│   └── db-admin.sh             # dynamodb-admin（GUI）をダミーキーで起動（CLI直接用）
├── env.json                    # ローカル用 DYNAMODB_ENDPOINT（gitignore済み。各自作成する）
├── Makefile
├── CLAUDE.md                   # プロジェクト指示書（先に読む）
└── IMPLEMENTATION.md           # このファイル
```

### ローカルで動かす
```bash
sam build

# 1回だけの呼び出し確認（例）
sam local invoke PostUsersFunction --event events/post-users.json --env-vars env.json
sam local invoke GetMeFunction --event events/get-me.json --env-vars env.json
sam local invoke PostInvitesFunction --event events/post-invites.json --env-vars env.json

# GET /invites/{token}, POST /invites/{token}/accept はトークンがレスポンスで動的に発行されるため、
# events/get-invite.json・events/post-invite-accept.json のプレースホルダー "REPLACE_WITH_REAL_TOKEN" を
# 実際のトークンに差し替えた一時ファイルを作って sam local invoke --event <一時ファイル> で呼ぶ。

# HTTPサーバとして立てて確認
sam local start-api --env-vars env.json
curl -X POST http://127.0.0.1:3000/users -H 'Content-Type: application/json' -d '{"displayName":"けんと","email":"kento@example.com"}'
curl http://127.0.0.1:3000/me
curl -X POST http://127.0.0.1:3000/invites
curl http://127.0.0.1:3000/invites/<token>
curl -X POST http://127.0.0.1:3000/invites/<token>/accept -H 'Content-Type: application/json' -d '{"displayName":"あい"}'
curl http://127.0.0.1:3000/kotozute/this-week
```
`env.json`は関数ごとに`DYNAMODB_ENDPOINT: "http://host.docker.internal:8000"`を指定する形式（gitignore対象。各自ローカルに作成。関数が増えたらキーを追加する）。`AWS_ACCESS_KEY_ID`等も書いてよいが、上記の理由により`sam local`実行時は無視されホストのdefaultプロファイルが使われる点に注意。

## 次にやること（順序）

### 1. 認証（Cognito）を被せる
- Cognito ユーザープール作成（メール＋パスワード標準フロー）。
- API Gateway に Cognito Authorizer。各ハンドラ内の固定値（`fixedUserID=k01`, `fixedPartnerUserID=k02`, `fixedPairID=p01`）を「トークンから取得」「PAIR METAのmember1/member2から解決」する形に差し替える。

### 2. 週一オープンのバッチ
- GSI① を作成（`scripts/create-table.sh` に GSI 定義を追加）。
- EventBridge Scheduler → Lambda。オープン処理＋通知の出し分け＋lastOpenedAt 更新。両者ゼロはスキップ。

### 3. 通知（SNS→APNs）／アーカイブのページング／PATCH系
- APNs 設定、SNS 連携。`GET /kotozute/archive` のカーソルページング。
- `PATCH /me`（表示名・APNsトークン更新）、`PATCH /kotozute/{id}`（編集）、`GET /pair`・`PATCH /pair`（オープン曜日・時刻の変更）も未実装。

## 設計の出典
全仕様は Notion「ことづて」設計ドキュメント（開発ページ配下）にある。矛盾があれば Notion を正とする。主要点は CLAUDE.md に転記済み。

## 注意点（過去に決めた重要な判断）
- フレームワーク（Gin等）は使わない。素の aws-lambda-go。
- 相互ロックはサーバー側判定・本文を返さない（セキュリティ）。
- 削除は実装しない。編集はオープン前・本文のみ。
- 気配は「存在のみ」、枚数は出さない、相手0枚は無言。
- 「今週」はカレンダー週でなく lastOpenedAt 境界。
- DynamoDBの予約語（`status`, `ttl`など）はUpdate/ConditionExpressionで直接使わず、必ず`ExpressionAttributeNames`でエイリアスする。
