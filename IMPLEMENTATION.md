# 実装状況と次のステップ

## 現在地（2026-08-16 時点・その3）

初回の本番デプロイと iOS からの実結線を行い、その過程で見つかった設計の不整合を直した。

### 本番デプロイ（ap-northeast-1・スタック名 `kotozute`）
- `template.yaml` に `AWS::DynamoDB::Table`（PK/SK・GSI1・PAY_PER_REQUEST）を追加。
  テーブルは `scripts/create-table.sh` で DynamoDB Local にしか作られておらず、
  デプロイしても本番にテーブルが無く API が 500 を返していた。
- デプロイ用の IAM ユーザーに EventBridge の権限が無く、週一バッチのルール作成で失敗していた
  （`AWSLambda_FullAccess` には `events:*` が含まれない）。インラインポリシーで付与。
- 認証の実挙動を確認：トークン無しは API Gateway が 401、`GET /invites/{token}` のみ公開。

### サイクル境界の設計を見直した（重要）
実データで通したところ、**オープンしたことづてが同じ瞬間に「今週」から出る**不整合が判明した。
`lastOpenedAt` 1つで「読むもの」と「書くもの」の両方の境界を兼ねていたため、
バッチがそれを前に進めると、いま解禁した分が範囲外になっていた。
結果として画面4状態のうち ③ロック と ④相手のことづてを見せる に到達できなかった
（＝相互ロックの「書けば見られる」側が一度も成立しない）。

**直し方**：ペアが区切りを2つ持つ（`cycleStartedAt` / `previousCycleStartedAt`）。
詳細は CLAUDE.md「サイクル境界」。あわせて **ことづての `opened` / `openedAt` を廃止**した。
オープン済みかどうかはペアの区切りとの比較で決まるので、1枚ごとに状態を持たせない。

- バッチは区切りを進める `UpdateItem` 1回だけになった（以前はことづて全件を更新）。
- `patch-kotozute` の「オープン前のみ編集可」は、ペアの区切りが動いていないことを
  条件にした `TransactWriteItems` で担保する（`opened=false` 条件の代わり）。
- `get-archive` の `whose=partner` は `createdAt < cycleStartedAt` で絞る。

**この変更は後方互換ではない**。旧スキーマのデータ（`lastOpenedAt` / `opened`）は読めないので、
本番の動作確認用データは削除して作り直した。実ユーザーはまだいない。

## 現在地（2026-08-10 時点・その2）
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
- **認証（Cognito）を全エンドポイントに導入済み。固定値 `k01`/`k02`/`p01` をコードから排除。**
  - `template.yaml` に `AWS::Cognito::UserPool`（メール＋パスワード、確認コードで本人確認、パスワード8文字以上）と `AWS::Cognito::UserPoolClient`（iOS用・公開クライアント・SRP/USER_PASSWORD/REFRESH_TOKEN 許可）を追加。`Outputs` に `UserPoolId`/`UserPoolClientId`。
  - `Globals.Api.Auth` に `CognitoAuthorizer`（`UserPoolArn` 指定）を定義し、エンドポイントごとに割り当て。`GET /invites/{token}` のみ公開（未登録の受諾者が「誰から」を見られる導線）、他はすべて認証必須。
  - 各ハンドラに `userIDFromRequest`（`request.RequestContext.Authorizer["claims"]["sub"]` を型アサーションで取り出す）を追加。トークンが無い/壊れていれば401（API Gatewayの門番に加えた多層防御）。
  - `pairID`・相手IDはクライアントから受け取らず、トークンの `userID` を起点に解決：`getUserPairID`（USER META の `pairId` を引く）、`partnerUserID`（PAIR META の member1/member2 から自分じゃない方を選ぶ純粋関数）。URLのIDを信用しない設計で他人のペアを構造的に覗けない。
  - `POST /invites/{token}/accept` は受諾者を `sub` に、成立するペアIDを `uuid.NewString()` の新規発行に変更（p01固定を廃止。get-this-week/post-kotozute が USER META から pairId を引くため自動追従）。
  - `post-kotozute` はチェック順を「認証→body検証→pairID解決(DB)」に整理（安い検証を先、DBアクセスを後）。ペア未成立は 409、`get-this-week` のペア未成立は空の今週状態を返す。
  - ローカルテスト用イベントJSONに認証claims（`authorizer.claims.sub`）を追加。認証追加で前提が変わった既存テストを `authedRequest`/`withClaims` ヘルパー経由に修正し、「トークン無し→401」テストと `partnerUserID` のテーブルドリブンテストを追加。全関数 `go build`／`go test` 通過。
  - **未確認**：`sam` がこの作業環境に無く、Cognito Authorizer はローカルでは検証されないため、実際の401挙動とデプロイ確認は未実施（ロジックのユニットテストとビルドのみ確認済み）。
- **残りAPI 5本を実装し、CLAUDE.md のエンドポイント一覧12本がすべて実装済みに。**
  - `GET /pair`（get-pair）：トークン→pairID→PAIR META を辿り、相手の userId/displayName・オープン設定・status・nextOpenAt を返す。相手の表示名は相手の USER META から取得。ペア未成立は404。
  - `PATCH /me`（patch-me）：表示名・APNsトークンの部分更新。`*string` で「未指定(nil)」と「空文字」を区別し、送られた項目だけ `UpdateExpression` に組み立てる（buildUpdateExpression）。`attribute_exists(PK)` で未登録は404（UpdateItem の upsert 抑止）。
  - `PATCH /pair`（patch-pair）：オープン曜日(0-6)・時刻(HH:MM 2桁)・タイムゾーン(time.LoadLocation で実在確認)を検証してから部分更新。不正値は DB 前に400。
  - `PATCH /kotozute/{id}`（patch-kotozute）：URLの id だけでは完全なSKを作れないため `begins_with(SK, "KOTOZUTE#<自分>#")` で検索し末尾idが一致する1件を特定。opened=true は編集不可(409)、本文のみ SET、`ConditionExpression opened=false` で検索と更新の間のオープン競合を防御。検索プレフィックスに自分のuserIDを埋めるため他人のことづては構造的に対象外。
  - `GET /kotozute/archive`（get-archive）：月ごと（year/month）のカーソルページング。`Limit`＋`ExclusiveStartKey`/`LastEvaluatedKey` を Base64 で不透明化して cursor 化（encodeCursor/decodeCursor、round-trip テスト済み）。whose=partner は FilterExpression opened=true のみ（過去ロックの緩和）。続きが無ければ nextCursor は null。
  - いずれも既存の認証パターン（userIDFromRequest・トークン起点の pairID/相手解決）を踏襲し、CognitoAuthorizer を適用。純粋関数（buildUpdateExpression/buildPairUpdate/parseArchiveQuery/encodeCursor 等）に切り出してテーブルドリブンでテスト。全12関数 `gofmt`/`go vet`/`go build`/`go test` 通過。
- **CI（GitHub Actions）を追加。** `.github/workflows/ci.yml` で main への push / PR 時に、関数ごとに gofmt・go vet・go build・go test を実行。
- **週一オープンのバッチ（weekly-open）を実装。**
  - GSI①（GSI1: PK=openDayOfWeek(N), Projection=ALL）を `scripts/create-table.sh` に追加。openDayOfWeek を持つ PAIR META だけが載るスパース索引。
  - `functions/weekly-open/`：EventBridge Schedule（`cron(0 * * * ? *)` 毎正時）で起動する `func(ctx) error` 型の Lambda。API Gateway からは呼ばれない。
  - GSI1 で openDayOfWeek=今日 のペアを検索 → `shouldOpen`（純粋関数）でペアの timezone 基準に判定：オープン曜日・時刻を過ぎ、かつ lastOpenedAt が今回のオープン時刻より前のときだけオープン。二重オープン防止により毎時走っても1サイクル1回（冪等）。
  - このサイクル分（createdAt>=lastOpenedAt, opened=false）を opened=true+openedAt に更新し lastOpenedAt を前進。各更新は opened=false 条件付き。両者ゼロでも lastOpenedAt は更新（静かにスキップ）。
  - 通知：`buildNotifications`（純粋関数）で出し分け（両者ゼロは無言／書いた人「オープンしました」／書いてない人「1つ書くと○○のことづても見られます」）。書いた人はオープン前の未オープンSKのプレフィックスから判定。`sendNotification` は USER META の snsEndpointArn へ SNS Publish、未登録ならログしてスキップ。通知失敗はオープンと切り離しログのみ。
  - `time/tzdata` を埋め込み Lambda 環境でも LoadLocation を効かせる。template.yaml に WeeklyOpenFunction（Schedule / Timeout 60 / DynamoDBCrudPolicy + sns:Publish）を追加。
  - **未実装（別タスク）**：APNs エンドポイントARN（snsEndpointArn）の登録導線が無いため、現状では通知は実際には飛ばずログのみ。マルチタイムゾーン厳密対応（バッチの曜日判定は Asia/Tokyo 基準）も将来課題。

### 重要な発見：`sam local invoke`/`start-api`はAWS認証情報系の環境変数を`--env-vars`で上書きできない
`DYNAMODB_ENDPOINT`のような独自の環境変数は`env.json`の指定どおりコンテナに渡るが、`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`/`AWS_REGION`は「Lambda予約環境変数」としてSAM CLIが実行ホストの `~/.aws/credentials` の `default` プロファイルから自動的に注入し、`env.json`の値やコマンドラインの`--profile`指定を上書きする（実機のLambda実行環境がIAMロールの認証情報を自動注入し、ユーザーが上書きできないのと同じ挙動をローカルでも再現している）。
- そのため、ホストマシンに実際のIAM認証情報（`~/.aws/credentials`の`[default]`）が設定されていると、`sam local invoke`から書き込まれるデータはそのIAMキー×リージョンのDynamoDB Local領域に入る。`env.json`に書いた`AWS_ACCESS_KEY_ID=local`は効かない。
- DynamoDB Localへの接続先自体（エンドポイント）は正しく`DYNAMODB_ENDPOINT`で制御できているので、本物のAWSに繋がってしまうわけではない（実害はない）。ただし「ダミーキーに統一」という当初の意図（IAMキーローテーション時にローカルデータが迷子にならないようにする）は、`sam local invoke`/`start-api`経由の書き込みに関しては実現できていない。
- `scripts/create-table.sh` / `scripts/db-admin.sh` はSAMを経由しない直接のCLI呼び出しなので、ここでのダミーキー統一は引き続き有効。
- 対応：この制約を認識した上で、`sam local invoke`でのテスト時はホストの`default`プロファイル（現状のIAMキー）がDynamoDB Localの実データ領域になることを前提にする。動作確認時にCLIでデータを直接見る/投入する場合も、ダミーキーではなくデフォルトプロファイル（環境変数を明示的にunset/exportしない状態）を使うこと。

## プロジェクト構成（現状）
```
kotozute/
├── template.yaml               # SAM。Cognito(UserPool/Client) + 12関数 + Authorizer を定義
├── .github/workflows/ci.yml    # CI：関数ごとに gofmt/vet/build/test
├── functions/
│   ├── post-users/             # POST /users
│   ├── get-me/                 # GET /me
│   ├── patch-me/               # PATCH /me
│   ├── post-invites/           # POST /invites
│   ├── get-invite/             # GET /invites/{token}（公開）
│   ├── post-invite-accept/     # POST /invites/{token}/accept
│   ├── get-pair/               # GET /pair
│   ├── patch-pair/             # PATCH /pair
│   ├── post-kotozute/          # POST /kotozute
│   ├── get-this-week/          # GET /kotozute/this-week
│   ├── patch-kotozute/         # PATCH /kotozute/{id}
│   ├── get-archive/            # GET /kotozute/archive
│   └── weekly-open/            # 週一オープンのバッチ（EventBridge起動、API外）
│       （各ディレクトリに main.go / main_test.go / go.mod / go.sum）
├── events/                     # sam local invoke 用サンプルイベント（関数ごと・認証claims入り）
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

### 1. 認証（Cognito）を被せる — ✅ 実装済み（デプロイ確認は残）
- ユーザープール作成・Authorizer 割り当て・固定値のトークン起点への差し替えは完了（詳細は「現在地」参照）。
- 残タスク：(a) `sam` のある環境でデプロイし、トークン無し→401・正規トークンで通ることを実地確認。(b) User Pool に `EmailConfiguration` を足して確認メールを SES 経由に切り替え（今は Cognito デフォルト送信・1日50通制限）。(c) iOS 側に `UserPoolId`/`UserPoolClientId` を渡してログインを構成。

### 2. 残りのAPI（PATCH系・GET /pair・アーカイブ） — ✅ 実装済み
- `PATCH /me`・`GET /pair`・`PATCH /pair`・`PATCH /kotozute/{id}`・`GET /kotozute/archive` を実装済み（詳細は「現在地」参照）。これで API は全12本が揃った。
- 残タスク：デプロイ後の実地確認（認証まわりと同様、`sam` のある環境で）。

### 3. 週一オープンのバッチ — ✅ 実装済み
- GSI①・バッチ本体・オープン処理・通知の出し分けまで実装済み（詳細は「現在地」参照）。
- 残タスク：デプロイして EventBridge の実起動・GSI クエリ・オープン挙動を実地確認。

### 4. 本番デプロイ・通知の実配信・その他（残り）
- **デプロイ全般**：`sam` のある環境で `sam build && sam deploy`。認証(401)・API・バッチの実挙動を確認。本番テーブル/GSI は現状 IaC 外なので、`template.yaml` への `AWS::DynamoDB::Table` 追加を検討。
- **通知の実配信**：APNs 証明書設定＋SNS プラットフォームアプリ作成。APNs トークン→SNS エンドポイントARN を作り USER META の `snsEndpointArn` に保存する導線（`PATCH /me` の apnsToken 受領時など）。これが入ると weekly-open の通知が実際に飛ぶ。
- **メール**：User Pool の確認メールを SES 経由に切り替え。
- **iOS**：`UserPoolId`/`UserPoolClientId` を渡してログイン構成。
- **GSI②（メール）**：Cognito 以外のログイン経路を作る場合に追加。

## 設計の出典
全仕様は Notion「ことづて」設計ドキュメント（開発ページ配下）にある。矛盾があれば Notion を正とする。主要点は CLAUDE.md に転記済み。

## 注意点（過去に決めた重要な判断）
- フレームワーク（Gin等）は使わない。素の aws-lambda-go。
- 相互ロックはサーバー側判定・本文を返さない（セキュリティ）。
- 削除は実装しない。編集はオープン前・本文のみ。
- 気配は「存在のみ」、枚数は出さない、相手0枚は無言。
- 「今週」はカレンダー週でなく lastOpenedAt 境界。
- DynamoDBの予約語（`status`, `ttl`など）はUpdate/ConditionExpressionで直接使わず、必ず`ExpressionAttributeNames`でエイリアスする。
