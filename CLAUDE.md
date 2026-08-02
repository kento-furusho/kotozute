# ことづて — Claude Code 向けプロジェクト指示書

親しいふたりが、日常の「いいな／ありがとう」をことづて（メッセージ）に書き溜め、週に一度まとめてお互いのことづてが見られるようになるアプリ。iOS（Swift）＋ AWS サーバーレスバックエンド。

このリポジトリはバックエンド（AWS SAM / Go）。詳細な仕様は Notion の設計ドキュメントにあるが、実装に必要な要点はこの文書と `IMPLEMENTATION.md` に集約している。

---

## 技術スタック（確定・変更しない）
- **API層**: API Gateway (HTTP API) + Lambda
- **Lambda 言語**: Go（`aws-lambda-go` の素ハンドラ。Web フレームワーク（Gin等）は使わない）
- **DB**: DynamoDB（Single Table Design、テーブル名 `kotozute`）
- **認証**: Cognito（メール＋パスワードの標準フロー）。API Gateway に Cognito Authorizer
- **メール送信**: SES
- **週一バッチ**: EventBridge Scheduler → Lambda
- **プッシュ通知**: SNS → APNs
- **IaC**: AWS SAM（`template.yaml`）
- **ローカル開発**: SAM Local + DynamoDB Local（Docker、`http://localhost:8000`）
- **アーキテクチャ**: arm64（Apple Silicon。`template.yaml` の Architectures は arm64）

**方針**: Amplify は使わず素の AWS を配線する（学習目的）。フレームワークを足さない。

---

## コーディング規約・進め方
- 1機能1関数（サーバーレスの粒度）。Lambda ハンドラは薄く保つ。
- DynamoDB クライアントは `init()` で1回だけ生成し使い回す。
- ローカルは環境変数 `DYNAMODB_ENDPOINT`（`http://localhost:8000` 等）で DynamoDB Local に向ける。未設定なら本物の AWS に接続する実装にする。
- 認証は段階的に導入。まずは `pairID`/`userID` を仮の固定値（`p01`/`k01`）で動かし、後で Cognito のトークンから取得する形に差し替える。
- セキュリティ要件：相互ロックの判定は必ずサーバー側で行い、条件を満たさなければ相手のことづて本文をレスポンスに含めない（クライアントで隠すだけは不可）。
- 権限チェックはサーバー側で。URL の ID を信用せず、トークンのユーザーと突き合わせる。

---

## データモデル（DynamoDB Single Table：テーブル名 `kotozute`）

メインテーブル（PK / SK は文字列）:

| PK | SK | 主な属性 |
|---|---|---|
| USER#<id> | META | email, displayName, apnsToken, pairId |
| PAIR#<id> | META | member1, member2, openDayOfWeek, openTime, timezone, status, lastOpenedAt |
| PAIR#<id> | KOTOZUTE#<書いた人id>#<ISO日時>#<ことづてid> | body, opened(bool), openedAt, createdAt |
| INVITE#<token> | META | inviterUserId, status, ttl |

GSI（2個）:
- **GSI①（曜日）**: openDayOfWeek を PK → 週一バッチが「今日オープンすべきペア」を検索
- **GSI②（メール）**: EMAIL#<addr> を PK → ログイン時にメールからユーザーを特定

補足:
- 「ユーザー→ペア」は USER の META に pairId を持たせて解決（1人1ペア前提、GSI不要）
- 「招待」はトークンで直引き（GetItem）のため GSI 不要

### 主なアクセスパターン
- 今週の自分のことづて: `PK=PAIR#<pair>, SK begins_with KOTOZUTE#<自分>#<今週範囲>`
- 今週の相手のことづて: 同上（相手ID）かつ opened=true
- アーカイブ（月ごと）: `SK begins_with KOTOZUTE#<誰か>#<年月>`（相手は opened=true のみ）
- ログイン: GSI② `PK=EMAIL#<addr>`
- バッチ対象ペア検索: GSI① `PK=openDayOfWeek=5`
- 自分の情報/ペア: `GetItem USER#<id>/META` → pairId 取得

---

## API エンドポイント一覧
認証必須（Cognito トークンをヘッダに）。URL に自分の ID やペア ID は基本入れない（トークンから取る）。

- `POST /users` — 初回ユーザー登録（表示名など）
- `GET /me` — 自分の情報＋ペア状態＋次のオープン日（起動時の起点）
- `PATCH /me` — 表示名・APNsトークンの更新
- `POST /invites` — 招待を作る（リンク発行）
- `GET /invites/{token}` — 招待の中身（誰から？）
- `POST /invites/{token}/accept` — 招待を受けてペア成立
- `GET /pair` — ペア情報
- `PATCH /pair` — オープン曜日・時刻の変更
- `POST /kotozute` — ことづてを書く
- `GET /kotozute/this-week` — 今週の状態をまとめて取る（相互ロック内包。下記）
- `PATCH /kotozute/{id}` — 編集（オープン前・本文のみ。サーバーで可否判定）
- `GET /kotozute/archive` — アーカイブ（カーソルページング）

### GET /kotozute/this-week のレスポンス（相互ロック内包）
```json
{
  "myKotozute": [ ... 自分の今週分（常に見える）... ],
  "openStatus": "before" | "opened",
  "canSeePartner": true,
  "partnerKotozute": [ ... ],
  "partnerHasWritten": true,
  "nextOpenAt": "2026-06-19T20:00"
}
```
- `canSeePartner=false` のとき `partnerKotozute` は空/省略（本文を返さない）
- `partnerHasWritten` は 0枚か1枚以上かのみ（枚数は返さない）

### GET /kotozute/archive
```
GET /kotozute/archive?whose=me|partner&year=2026&month=6&cursor=<不透明トークン>&limit=10
→ { "items": [...], "nextCursor": "<続き>" | null }
```
- Dynamo の LastEvaluatedKey を Base64 等で不透明化して cursor に使う
- 同じ月内のみ。`whose=partner` は opened=true のみ

---

## コアロジック（実装時の必読）

### 相互ロック判定（GET /kotozute/this-week 内）
```
自分が相手の今週分を見れる
  ⟺ 相手の今週分が opened=true（バッチ解禁済み） かつ 自分が今週1枚以上書いている
```
分岐（自分視点）:
| 自分の今週分 | 相手のopened | 画面 |
|---|---|---|
| 1枚以上 | あり | 相手のことづてを見せる |
| 1枚以上 | なし | オープン前の待ち |
| 0枚 | あり | ロック（書くと見れる） |
| 0枚 | なし | 通常ホーム |

気配表示：相手0枚なら気配・ロック要素を一切出さない（`partnerHasWritten=false`）。枚数は返さない。

### サイクル境界（「今週」の定義）
- カレンダー週ではなく **オープン日を境界**にする。ペアの META に `lastOpenedAt`（前回オープン日時）を持つ。
- 今週の範囲 = `lastOpenedAt 〜 現在`。バッチがオープンしたら `lastOpenedAt` を更新。
- 初回：ペア成立（invite accept）時に `lastOpenedAt = ペア成立日時` で初期化。

### 週一オープン処理（バッチ）
```
EventBridge(曜日×時刻) → Lambda
  → GSI①で openDayOfWeek/openTime のペアを検索
  → 各ペアの今週分ことづてを opened=true に更新（openedAt 記録）
  → lastOpenedAt を更新
  → 2人へ SNS→APNs で通知（下記の出し分け）
```
- **両者ゼロの週**：opened にする対象なし。通知しない。lastOpenedAt は更新して次サイクルへ（静かにスキップ）。
- **二重オープン防止**：lastOpenedAt を見て「前回から1サイクル経ってなければオープンしない」。

### 通知の出し分け
- 今週書いた人 → 「今週のことづて、オープンしました」
- 今週書いてない人 → 「あなたが1つ書くと、○○のことづても見られます」（催促でなく事実）
- 両者ゼロの週は通知しない。

### 過去ロックの緩和（現在は厳格・過去は寛容）
- 今週分：厳格（相互ロック）
- アーカイブ（過去分）：opened=true なら見られる（自分がその週書いたかは問わない）。実装は `GET /kotozute/archive?whose=partner` を opened=true で絞るだけ。

### 状態遷移・編集/削除
- ことづて：書く →（オープン前：下書き、本人のみ、編集可）→ 週一オープン → 確定（変更不可）
- **編集**：オープン前のみ、本文のみ。サーバーで可否判定。
- **削除**：実装しない（状態遷移を一方向に保つ）。

---

## 用語（UI/コメントで統一）
- **ことづて**：書いたメッセージ1つ1つ（単複同表記）
- **オープン**：週に一度、相手のことづてが見られるようになること／その日
- 「開封」「めくる」「届く」は使わない

---

## デザイン成果物（`docs/design/`）
UI デザインは `docs/design/` に配置。画面のコード（Claude Design で作成した HTML）とスライドがある。フロント実装や、API のレスポンス設計で画面と突き合わせる際はここを参照する。

デザイン確定事項（実装時に守る）:
- モノの見た目は**付箋**。ただし呼称・動詞は「ことづて」「オープン」で統一
- トーンは**明るい・軽い**（ポップ寄り）
- 相手の気配は「存在のみ」表示、枚数は出さない、相手0枚は無言
- UI 文言：タイトルは名詞で短く（「今週のことづて」「◯◯からのことづて」）、説明は常体でやわらかく。「開封／めくる／届く」および見出し・ボタンとしての「見れるようになる」は使わない
- 画面遷移：はじめる → 登録（メールのみ）→ オンボーディング（コンセプト2枚）→ 最初の1枚を書く → 招待。される側は別導線

---

## ローカル開発コマンド

### 認証情報はダミーキーに統一する
DynamoDB Local は「リージョン × アクセスキー」ごとにデータ領域が分かれる。本番の IAM キーをローカル接続に使うと、キーをローテーションしたときにローカルのテーブルが迷子になるため、ローカル操作（CLI・Lambda ローカル実行のどちらも）は必ず以下のダミー認証情報で統一する。本番キーはローカルには一切使わない。
```
AWS_ACCESS_KEY_ID=local
AWS_SECRET_ACCESS_KEY=local
AWS_REGION=ap-northeast-1
```

```bash
# DynamoDB Local 起動（Docker）
docker run -d -p 8000:8000 --name dynamodb-local amazon/dynamodb-local

# テーブル作成・GUI確認は scripts/ のスクリプトを使う（ダミーキー・実行権限付与済み）
./scripts/create-table.sh   # メインテーブル kotozute（PK/SK）を作成
./scripts/db-admin.sh       # dynamodb-admin（GUI）を起動

# テーブル確認
AWS_ACCESS_KEY_ID=local AWS_SECRET_ACCESS_KEY=local AWS_REGION=ap-northeast-1 \
  aws dynamodb list-tables --endpoint-url http://localhost:8000

# ビルド & ローカル実行（env.json でダミーキーと DYNAMODB_ENDPOINT を渡す。詳細は IMPLEMENTATION.md）
sam build
sam local invoke --env-vars env.json
sam local start-api --env-vars env.json   # http://127.0.0.1:3000 でAPIを立てる
```

**未対応**：GSI①②はまだ `scripts/create-table.sh` に定義していない。GSI を使う機能（ログイン、バッチ）実装時に、このスクリプトへ GSI 定義を追加すること。