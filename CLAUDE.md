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
| USER#<id> | KOTOZUTE#<ISO日時>#<ことづてid> | body, createdAt, color |
| PAIR#<id> | META | member1, member2, openDayOfWeek, openTime, timezone, status, cycleStartedAt, previousCycleStartedAt, cycleStarts |
| INVITE#<token> | META | inviterUserId, status, ttl |
| DEVICE#<APNsトークン> | META | userId（この端末の持ち主） |

GSI（2個）:
- **GSI①（曜日）**: openDayOfWeek を PK → 週一バッチが「今日オープンすべきペア」を検索
- **GSI②（メール）**: EMAIL#<addr> を PK → ログイン時にメールからユーザーを特定

補足:
- **ことづては書いた人の下に置く**（`USER#<書いた人>`）。ペアの下ではない。理由は2つ:
  - **ペア成立前にも書ける**。オンボーディングで「最初の1枚」を書いてから招待する導線が成立する
  - **所有者がキーで表現される**。自分のパーティションしか引けないので、他人のことづてを編集する経路が構造的に存在しない
- 「ユーザー→ペア」は USER の META に pairId を持たせて解決（1人1ペア前提、GSI不要）
- 「招待」はトークンで直引き（GetItem）のため GSI 不要
- **端末は最後に登録したアカウントだけのもの**（`DEVICE#<トークン>`）。`PATCH /me` で端末を登録するとき、
  前の持ち主が別人ならそちらから `apnsToken`/`snsEndpointArn` を外す。`apnsToken: ""` は「外す」（サインアウト用）。
  これが無いと、同じ端末でアカウントを切り替えたとき**前のアカウント宛ての通知が次の人の端末に届いた**
  （通知文には相手の名前が入る）

### 主なアクセスパターン
- 書くサイクルの自分のことづて: `PK=USER#<自分>, SK BETWEEN KOTOZUTE#<cycleStartedAt> AND KOTOZUTE#￿`
- 読むサイクルの相手のことづて: `PK=USER#<相手>, SK BETWEEN KOTOZUTE#<previousCycleStartedAt> AND KOTOZUTE#<cycleStartedAt>`
- アーカイブ（月ごと）: `PK=USER#<誰か>, SK begins_with KOTOZUTE#<年月>`（相手は `[cycleStarts[0], previousCycleStartedAt)` のみ）
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
- `POST /kotozute` — ことづてを書く（**ペア未成立でも書ける**）。`color` で付箋の色を指定できる
- `GET /kotozute/this-week` — 今週の状態をまとめて取る（相互ロック内包。下記）
- `PATCH /kotozute/{id}` — 編集（オープン前・本文のみ。サーバーで可否判定）
- `GET /kotozute/archive` — アーカイブ（カーソルページング）

### GET /kotozute/this-week のレスポンス（相互ロック内包）
```json
{
  "myKotozute": [ ... 書くサイクルの自分の分（常に見える）... ],
  "openStatus": "before" | "opened",
  "canSeePartner": true,
  "lastCycle": {
    "mine":    [ ... 直前のサイクルの自分の分 ... ],
    "partner": [ ... 直前のサイクルの相手の分 ... ]
  },
  "partnerHasWritten": true,
  "nextOpenAt": "2026-06-19T20:00"
}
```
- `lastCycle` は直前にオープンされたサイクル。**次のオープンまで手元で読める**（アーカイブにも残る）
- `lastCycle.mine` は自分のものなので常に返す。`canSeePartner=false` のとき `lastCycle.partner` は**キーごと省略**（本文を返さない）
- `partnerHasWritten` は「相手が**書くサイクル**に1枚以上書いたか」。`partnerWrittenCount` はその枚数
  （本文は返さない。本文を出してよいかは `canSeePartner` が決める）

### GET /kotozute/archive
```
GET /kotozute/archive?whose=me|partner&year=2026&month=6&cursor=<不透明トークン>&limit=10
GET /kotozute/archive?whose=me|partner&from=<ISO>&to=<ISO>          # 週ごとに引くとき（to は含まない）
→ { "items": [...], "nextCursor": "<続き>" | null }
```
- Dynamo の LastEvaluatedKey を Base64 等で不透明化して cursor に使う
- `year`/`month` は同じ月内のみ。`from`/`to` は月をまたいでよい（週の範囲を渡す）
- `from`/`to` は UTC に揃えてから使う。SK の比較は文字列の辞書順なので、書き方が違うと範囲がずれる
- `whose=partner` は `[cycleStarts[0], previousCycleStartedAt)` のみ。上限は読むサイクル（相互ロックの対象）を、
  下限は前の関係で書かれた分を、それぞれ落とすため
- `items` が `limit` に達したら `nextCursor` が付く。**続きは必ずたどること**。
  とくに `whose=partner` は絞り込みが `limit` の後に効くので、`items` が少なくても続きが残っていることがある

---

## コアロジック（実装時の必読）

### 相互ロック判定（GET /kotozute/this-week 内）
```
自分が相手のことづてを見れる
  ⟺ 読むサイクルに相手のことづてが1枚以上ある
     かつ 自分が「読むサイクル」か「書くサイクル」のどちらかに1枚以上書いている
```
自分側で2つのサイクルを見るのは意図的:
- **読むサイクルに書いた** … その週ちゃんと書いた人。オープン直後にそのまま読める（通知文「今週のことづて、オープンしました」と一致）
- **書くサイクルに書いた** … その週は書かなかったが、いま1枚書いた人。これで解禁される（通知文「あなたが1つ書くと、○○のことづても見られます」と一致）
分岐（自分視点）:
| 自分の今週分 | 相手のopened | 画面 |
|---|---|---|
| 1枚以上 | あり | 相手のことづてを見せる |
| 1枚以上 | なし | オープン前の待ち |
| 0枚 | あり | ロック（書くと見れる） |
| 0枚 | なし | 通常ホーム |

気配表示：相手0枚なら気配・ロック要素を一切出さない（`partnerHasWritten=false`）。
枚数は `partnerWrittenCount` で返し、ぼかした付箋をその数だけ並べる。

### サイクル境界（区切りは2つ持つ）
カレンダー週ではなく **オープン日を境界**にする。ペアの META に区切りを **2つ** 持つ。

```
previousCycleStartedAt        cycleStartedAt              現在
        │                            │                     │
        │←──── 読むサイクル ────→│←── 書くサイクル ──→│
        │   （オープン済み）      │   （次のオープン待ち）  │
```

- **読むサイクル** = `[previousCycleStartedAt, cycleStartedAt)`。直前に閉じた分。相手のことづてはここから返す。
- **書くサイクル** = `[cycleStartedAt, 現在)`。いま書いている分。自分のことづては基本ここ。
- 初回：ペア成立（invite accept）時に両方を **遠い過去（1970-01-01）** で初期化する。
  読むサイクルは空（＝オープン前）になり、**ペア成立前に書いた分も最初の書くサイクルに入る**。
  成立日時で初期化すると、オンボーディングで書いた「最初の1枚」が最初のサイクルから外れてしまう。
  ただし**下限はふたりの `lastPairEndedAt` まで上げる**（`initialCycleStart`）。
  遠い過去のままだと、前の相手に向けて書いた分が次の相手の最初のサイクルに入る。

**区切りの履歴（`cycleStarts`）**：オープンのたびに開始時刻を積んでいく（古い順）。
隣り合う2つが1週間の範囲になる。直近2つだけでは過去の週を復元できず、
壁を週ごとにたどれないため。カレンダーの週で代用すると、オープン曜日を変えたときにずれる。

**区切りを2つにする理由**：1つだと、オープンした瞬間に「読むもの」と「書くもの」の区別がつかない。
オープンでその1つの区切りを前に進めると、いま解禁したことづてが同時に範囲外へ出てしまい、
④「相手のことづてを見せる」に永久に到達できなくなる（実際にそうなっていた）。

**ことづてに `opened` フラグは持たせない**。あることづてがオープン済みかどうかは
「書かれた時刻が `cycleStartedAt` より前か」で決まり、ペアの区切りだけで判定できる。
状態を1箇所に集めることで、「アイテムは opened なのに範囲外」という食い違いが起こり得なくなる。

### 週一オープン処理（バッチ）
```
EventBridge(曜日×時刻) → Lambda
  → GSI①で openDayOfWeek/openTime のペアを検索
  → 区切りを1つずつ前へ進める（UpdateItem 1回）
       previousCycleStartedAt ← cycleStartedAt
       cycleStartedAt         ← 現在
  → 2人へ SNS→APNs で通知（下記の出し分け）
```
- **ことづては1枚も書き換えない**。オープンはペアの区切りを進めるだけ（1ペアあたり UpdateItem 1回）。
- **両者ゼロの週**：通知しない。区切りは進めて次サイクルへ（静かにスキップ）。
- **二重オープン防止**：`cycleStartedAt` が今回のオープン時刻以降なら処理済みとみなす。
- 通知の出し分けに「誰が書いたか」が要るので、閉じるサイクルのことづては読む（更新はしない）。

### 通知の出し分け
**送るかどうかは「相手が書いたか」で決める。自分が書いたかではない。**
- 相手が0枚 → その人には**送らない**（開いても読めるものが増えていない）。
  自分が書いたかで決めていたため、**自分だけ書いた週に「オープンしました」が飛んでいた**
- 相手が1枚以上 → 文面は自分が書いたかで分ける
  - 自分も書いた → 「今週のことづて、オープンしました」
  - 自分は書いてない → 「あなたが1つ書くと、○○のことづても見られます」（催促でなく事実）
- 結果として、両者ゼロの週は誰にも送らない。

### 過去ロックの緩和（現在は厳格・過去は寛容）
- **読むサイクル**（いま解禁されている分）：厳格（相互ロック）。`GET /kotozute/this-week` だけが返す。
- **それより前**（読み終えたサイクル）：寛容。オープン済みなら見られる（自分がその週書いたかは問わない）。

実装は `GET /kotozute/archive?whose=partner` を
**`[cycleStarts[0]`（いまのペアが始まった時刻）`, previousCycleStartedAt)`** で絞る。**上下とも要る。**

- **上限**：`cycleStartedAt` で絞ると読むサイクルまで入り、書いていない人がアーカイブ経由で
  相互ロックを迂回して読めてしまう（一度そうなっていた）。
- **下限**：上限だけにすると、ペア解消のあとに組んだ相手が「解消時刻より前」の条件を満たし、
  **前の関係で書かれたことづてを全部読めた**（これもそうなっていた）。
  this-week 側は `initialCycleStart` で区切りの下限を上げて塞いであったが、
  アーカイブは別経路なので同じ穴が残っていた。規約・プライバシーポリシーに書いた
  「解消より前に書いた分は、以後に組む相手には表示されません」と食い違う。

**教訓：相手の本文を返す経路は2つある**（`this-week` と `archive`）。
片方を塞いだら、必ずもう片方も同じ条件で塞ぐ。

### 付箋の色（ことづてごと）
色は**ことづてに付く**。書いた人が1枚ごとに選び、相手の画面でもその色で出る。
人に紐づく設定ではない。

- 保存するのは**6桁の16進数を1つだけ**（`"FFDC72"`）。先頭の `#` は落とし、大文字に揃える。
  紙のグラデーションの明るいほうは、クライアントが白を 0.34 混ぜて作る
- **選んだときだけ属性を置く。** 空文字を入れると「黒を選んだ」と区別がつかない。
  色を持たないことづては、画面が並び順から決める（既存のデータはこれ）
- 書き直し（`PATCH /kotozute/{id}`）は **省略＝触らない／`"none"`＝外す**。
  省略を「消す」にすると、本文だけ直したいときに色まで失われる

### 状態遷移・編集/削除
- ことづて：書く →（オープン前：下書き、本人のみ、編集可）→ 週一オープン → 確定（変更不可）
- **編集**：書くサイクル（`createdAt >= cycleStartedAt`）のもののみ、本文のみ。サーバーで可否判定。
- **削除**：実装しない（状態遷移を一方向に保つ）。

---

## 用語（UI/コメントで統一）
- **ことづて**：書いたメッセージ1つ1つ（単複同表記）
- **オープン**：週に一度、相手のことづてが見られるようになること／その日
- 「開封」「めくる」は使わない
- 「届く」も使わない（ことづては運ばれてこない。もうそこにあって、条件を満たすと読める）。
  ただし**オンボーディングの誘い文句だけは「送ろう」を使っている**。
  比喩の厳密さより、はじめて開いた人への分かりやすさを取った

---

## デザイン成果物（`docs/design/`）
UI デザインは `docs/design/` に配置。画面のコード（Claude Design で作成した HTML）とスライドがある。フロント実装や、API のレスポンス設計で画面と突き合わせる際はここを参照する。

デザイン確定事項（実装時に守る）:
- モノの見た目は**付箋**。ただし呼称・動詞は「ことづて」「オープン」で統一
- トーンは**明るい・軽い**（ポップ寄り）
- 相手の気配は**ぼかした付箋**を枚数ぶん並べて見せる（相手0枚は無言）。
  本文は出さない。ぼかしの下は線の帯だけで、偽の文章は敷かない
- UI 文言：タイトルは名詞で短く、説明は常体でやわらかく。「開封／めくる／届く」および
  見出し・ボタンとしての「見れるようになる」は使わない。
  **例外はオンボーディングの2枚**（「ことづてを送ろう。」「お互いに書かないと、見れないよ。」）。
  ここだけは声をかける調子にしてある。他の画面に持ち込まない
- **「壁に貼る」の比喩は文の中では使わない。** ボタンの「貼っておく」は動作の名前なので残す。
  文で使うと、付箋というモノの話と、ことづてという呼称が混ざる
- **確認モーダルは説明ではなく確認文。** 敬語で「本当に◯◯しますか？」と聞く。
  起きることの説明を並べない（画面を見れば分かるし、長いほど読まれない）。
  取り返しがつかないときだけ一言添える（「本当に削除しますか？この操作は取り消せません。」）
- **エラーや案内で、相手に何かをしてもらうときは丁寧語。**
  「招待した方に、新しいリンクを送ってもらってください。」のように、次に取る行動を書いて頼む。
  常体で「〜してほしい」と書くと、命令にも独り言にも読めて落ち着かない
- **常体の例外**：確認モーダル・エラーや案内のお願い・オープンの演出・通知は丁寧語にする
  （「◯◯のことづてが読めるようになりました」「今週のことづて、オープンしました」）。
  改まったほうがよい場面だけ丁寧語にする。日常の画面は常体のまま。
  閉じるボタンは「閉じる」（漢字）で統一する
- 見出しのことづては、自分側・相手側とも**名前で揃える**（「ひとりのことづて」「あいてのことづて」）。
  「◯◯から」は「◯◯からの招待」のように、ことづて以外を指すときには使ってよい
- 画面遷移：オンボーディング（2枚）→ はじめる → 登録 → 最初の1枚を書く → 招待。される側は別導線。
  **説明は登録より前に済ませる**（何のアプリか分からないままメールを入れさせない）

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

**GSI について**：GSI①（曜日）は `scripts/create-table.sh` と `template.yaml` の両方に定義済み。
GSI②（メール）は、Cognito 以外のログイン経路を作るときに追加する（今はコードから参照していない）。

既にテーブルがある環境に GSI を後から足すときは、作り直さず `update-table` を使う:
```bash
AWS_ACCESS_KEY_ID=local AWS_SECRET_ACCESS_KEY=local AWS_REGION=ap-northeast-1 \
aws dynamodb update-table --table-name kotozute --endpoint-url http://localhost:8000 \
  --attribute-definitions AttributeName=openDayOfWeek,AttributeType=N \
  --global-secondary-index-updates '[{"Create":{"IndexName":"GSI1","KeySchema":[{"AttributeName":"openDayOfWeek","KeyType":"HASH"}],"Projection":{"ProjectionType":"ALL"}}}]'
```
