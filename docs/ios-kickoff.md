# iOS クライアント キックオフ資料

ことづての iOS アプリ（Swift）を新規に立ち上げるための引き継ぎ資料。バックエンド（このリポジトリ）は実装・テスト済みで、本資料は「iOS からどう叩くか」をまとめたもの。

- バックエンドの全仕様は `CLAUDE.md`、実装状況は `IMPLEMENTATION.md`。矛盾があれば Notion 設計ドキュメントを正とする。
- iOS は**別リポジトリ**で開発する。ビルド・実行には macOS + Xcode が必要（このバックエンド用の Linux 環境では iOS はビルドできない）。

---

## 0. まず必要な設定値（デプロイ後に確定）

以下はバックエンドを `sam deploy` した後に得られる。iOS の設定（例：`Config.swift` や xcconfig）に入れる。

| 値 | 取得元 | 用途 |
|---|---|---|
| `UserPoolId` | SAM Outputs `UserPoolId` | Cognito ログイン |
| `UserPoolClientId` | SAM Outputs `UserPoolClientId` | Cognito ログイン（iOS 用クライアント） |
| API ベースURL | SAM Outputs / API Gateway | 例 `https://xxxx.execute-api.ap-northeast-1.amazonaws.com/Prod` |
| リージョン | `ap-northeast-1` | Cognito / API |

**未デプロイならまず `sam build && sam deploy --guided`。** それまで認証は実結線できない（UI は先行して作れる）。

---

## 1. 認証フロー（Cognito）

1. **サインアップ**：メール＋パスワードで Cognito にユーザー登録 → 確認コードがメールで届く → コード確認。
2. **サインイン**：メール＋パスワードでログイン → **IdToken（JWT）** を取得。
3. **API 呼び出し**：全リクエストの HTTP ヘッダに `Authorization: Bearer <IdToken>` を付ける。
4. **トークン更新**：IdToken は数時間で失効。RefreshToken で再取得（SDK が面倒を見る）。
5. **初回プロフィール登録**：サインイン後に `POST /users` で `displayName` を登録（Cognito はメール/パスワードのみ管理、表示名はアプリ側）。

### Amplify か 素の AWS SDK か（要判断）
- バックエンドは「Amplify を使わず素の AWS を配線」方針（学習目的）。iOS も揃えるなら **AWS SDK for Swift で素の Cognito**。
- ただし iOS の素 Cognito 実装は手間が大きい。**Amplify (Auth) を使うと大幅に楽**。学習方針と現実の工数のトレードオフで決める。→ 実装前に方針を決めること。

### サーバー側の認証の効き方（クライアントが前提にすること）
- 認証必須エンドポイントは、トークンが無い/不正なら **API Gateway が 401** を返す（Lambda に届かない）。
- サーバーは URL の ID を信用せず、**トークンの `sub`（ユーザーID）を起点に** pairID や相手を解決する。したがってクライアントは自分や相手の ID を URL に載せる必要はない（載せても無視される）。
- `GET /invites/{token}` のみ**公開**（未登録でも「誰からの招待か」を見られる導線）。それ以外は全て認証必須。

---

## 2. エンドポイント一覧（メソッド / パス / 認証）

| メソッド | パス | 認証 | 概要 |
|---|---|---|---|
| POST | `/users` | 必須 | 初回プロフィール登録（表示名） |
| GET | `/me` | 必須 | 自分の情報＋ペア状態＋次のオープン日（起動起点） |
| PATCH | `/me` | 必須 | 表示名・APNsトークンの更新 |
| POST | `/invites` | 必須 | 招待リンク（トークン）を作る |
| GET | `/invites/{token}` | **公開** | 招待の中身（誰から） |
| POST | `/invites/{token}/accept` | 必須 | 招待を受けてペア成立 |
| GET | `/pair` | 必須 | ペア情報 |
| PATCH | `/pair` | 必須 | オープン曜日・時刻の変更 |
| POST | `/kotozute` | 必須 | ことづてを書く |
| GET | `/kotozute/this-week` | 必須 | 今週の状態（相互ロック内包） |
| PATCH | `/kotozute/{id}` | 必須 | 編集（オープン前・本文のみ） |
| GET | `/kotozute/archive` | 必須 | アーカイブ（月ごと・ページング） |

---

## 3. 各エンドポイントの Request / Response

JSON。日時は基本 ISO8601。`?` 付きフィールドは省略されうる。

### POST /users
- Req: `{ "displayName": "けんと", "email": "kento@example.com" }`（displayName 必須）
- Res 201: `{ "userId": "...", "displayName": "けんと", "email": "..." }`
- 409: 既に登録済み

### GET /me
- Res 200: `{ "userId", "displayName", "email"?, "pairId"?, "pairStatus": "none"|"paired", "nextOpenAt"? }`
- `pairStatus="none"` ならまだペア未成立（招待導線へ）。`"paired"` ならホームへ。
- 404: ユーザー未登録（＝先に POST /users が必要）

### PATCH /me
- Req: `{ "displayName"?: "新名", "apnsToken"?: "..." }`（送った項目だけ更新。両方省略は 400）
- Res 200: `{ "userId", "displayName"?, "apnsToken"? }`
- 404: 未登録

### POST /invites
- Req: なし（空ボディで可）
- Res 201: `{ "token": "...", "expiresAt": "2026-..." }`（7日間有効）
- 400: 未登録 / 409: 既にペア済み

### GET /invites/{token}（公開）
- Res 200: `{ "inviterDisplayName": "けんと", "status": "pending" }`
- 404: 見つからない or 期限切れ（存在有無を漏らさない）

### POST /invites/{token}/accept
- Req: `{ "displayName"?: "あい" }`（受諾者が未登録なら表示名をここで渡せる）
- Res 200: `{ "pairId": "..." }`（ペア成立）
- 400: 自分の招待を受諾 / 409: 期限切れ・使用済み・既ペア

### GET /pair
- Res 200:
  ```json
  {
    "pairId": "...",
    "partner": { "userId": "...", "displayName": "あい" },
    "openDayOfWeek": 5,
    "openTime": "20:00",
    "timezone": "Asia/Tokyo",
    "status": "active",
    "nextOpenAt": "2026-08-14T20:00"
  }
  ```
- 404: ペア未成立

### PATCH /pair
- Req: `{ "openDayOfWeek"?: 5, "openTime"?: "20:00", "timezone"?: "Asia/Tokyo" }`
  - openDayOfWeek: 0(日)〜6(土) / openTime: "HH:MM"（2桁） / timezone: IANA 名
- Res 200: `{ "pairId", "openDayOfWeek"?, "openTime"?, "timezone"? }`
- 400: 値が不正 / 404: ペア未成立

### POST /kotozute
- Req: `{ "body": "今日はありがとう" }`（body 必須）
- Res 201: `{ "id": "...", "body": "...", "opened": false, "createdAt": "2026-..." }`
- **ペア未成立でも書ける**。ことづては書いた人の下に保存されるため。
  オンボーディングで「最初の1枚」を書いてから招待する導線のため。

### GET /kotozute/this-week（相互ロック内包・画面の中心）
- Res 200:
  ```json
  {
    "myKotozute": [ { "id", "body", "createdAt" }, ... ],
    "openStatus": "before" | "opened",
    "canSeePartner": true,
    "lastCycle": {
      "mine":    [ { "id", "body", "createdAt" }, ... ],
      "partner": [ ... ]
    },
    "partnerHasWritten": true,
    "nextOpenAt": "2026-08-14T20:00"
  }
  ```
- `myKotozute` は**いま書いているサイクル**の自分の分。
- `lastCycle` は**直前にオープンされたサイクル**。次のオープンまで手元で読める（アーカイブにも残る）。
  - `lastCycle.mine` は自分のものなので常に返る。
  - **`canSeePartner=false` のとき `lastCycle.partner` はキーごと省略される**（本文はサーバーが返さない。クライアントで隠すのではなく、そもそも来ない）。
- `partnerHasWritten` は「相手が**書くサイクル**に1枚以上書いたか」のみ（**枚数は返さない**）。相手0枚なら気配を一切出さないこと。
- 画面分岐は **`canSeePartner` を起点に**決める。自分の枚数から推論しないこと。
  「前のサイクルに書いた人は、いまのサイクルが0枚でも読める」ため、`myKotozute` が空でも
  `canSeePartner=true` になりうる。枚数で判断すると、その人をロック画面へ送ってしまう。

  | 条件 | 画面 |
  |---|---|
  | `canSeePartner=true` | 相手のことづてを見せる |
  | `openStatus="opened"` かつ見せられない | ロック（書くと見られる） |
  | `myKotozute` が1枚以上 | オープン前の待ち |
  | それ以外 | 通常ホーム（気配を出さない） |

### PATCH /kotozute/{id}
- Req: `{ "body": "書き直した本文" }`
- Res 200: `{ "id", "body" }`
- 409: 既にオープン済み（編集不可） / 404: 見つからない（他人のものは対象外）
- **編集はオープン前・本文のみ**。削除は無い。

### GET /kotozute/archive
- Query: `?whose=me|partner&year=2026&month=6&limit=10&cursor=<不透明トークン>`
  - whose 省略時は `me`。limit 既定10・上限50。
- Res 200: `{ "items": [ { "id", "body", "createdAt" }, ... ], "nextCursor": "<続き>" | null }`
- `nextCursor` を次リクエストの `cursor` にそのまま渡すと続きが取れる。中身は不透明（解釈しない）。`null` なら終端。
- `whose=partner` は **opened=true のみ**（過去ロックの緩和。過去分はオープン済みなら見られる）。

---

## 4. 画面遷移（design/ と対応）

- **始める → 登録（メールのみ）→ オンボーディング（コンセプト2枚）→ 最初の1枚を書く → 招待**
- **される側は別導線**：招待リンク → `GET /invites/{token}` で「◯◯からの招待」表示 → 登録/ログイン → `POST /invites/{token}/accept`。
- 起動時は `GET /me` を起点に、`pairStatus` で「招待導線」か「ホーム」を分岐。
- ホームは `GET /kotozute/this-week` の状態で画面を出し分け（上表）。

---

## 5. デザイン / UI 文言の決めごと（必守）

- モノの見た目は**付箋**。ただし呼称・動詞は「**ことづて**」「**オープン**」で統一。
- トーンは明るい・軽い（ポップ寄り）。
- 相手の気配は「存在のみ」表示、**枚数は出さない**、相手0枚は無言。
- 用語：「開封」「めくる」「届く」は使わない。見出し・ボタンとしての「見れるようになる」も使わない。
- タイトルは名詞で短く（「今週のことづて」「◯◯からのことづて」）、説明は常体でやわらかく。
- デザイン成果物は `docs/design/`（付箋デザイン案・オンボーディング）。

---

## 6. 用語

- **ことづて**：書いたメッセージ1つ1つ（単複同表記）。
- **オープン**：週に一度、相手のことづてが見られるようになること／その日。

---

## 7. 実装の進め方（提案）

1. まずバックエンドをデプロイし、設定値（UserPoolId 等）を確定。
2. iOS プロジェクト作成（SwiftUI）。API のレスポンスに対応する `Codable` なモデルを定義（この資料の Response 節がそのまま型になる）。
3. 認証（Amplify or 素 SDK）を組み、`Authorization: Bearer <IdToken>` を付ける共通の API クライアント層を作る。
4. `GET /me` → 分岐 → 各画面。相互ロックは `this-week` のレスポンスに従うだけ（判定はサーバー済み）。
5. 通知（APNs）は後半。`PATCH /me` で apnsToken を送る導線 → バックエンドの SNS エンドポイント登録（別タスク）と接続。

---

## 8. 現時点の未完（バックエンド側の残り）

iOS 開発中に影響しうる、バックエンドの未完了事項：

- **未デプロイ**：認証の実挙動・API 実 URL はデプロイ後に確定。
- **通知の実配信**：APNs エンドポイントARN 登録の導線が未実装のため、`PATCH /me` の apnsToken は保存されるが、週一バッチの通知はまだ実際には飛ばない（ログのみ）。
- **確認メール**：現状 Cognito デフォルト送信（1日50通・汎用差出人）。本番は SES へ切替予定。
- 詳細は `IMPLEMENTATION.md`「次にやること 4」。
