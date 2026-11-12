# Apple / Google ログインの準備（手作業の分）

Apple と Google の管理画面で行う設定。ここが済めば、サーバー（`template.yaml`）とアプリの実装に進める。

方針（2026-11-12 決定）:
- Cognito の連携（フェデレーション）を使う。アプリのボタンから Web シートで Apple / Google のログインへ直行し、
  終わったら Cognito がいつもと同じ形のトークンを返す。API 側の権限チェックは変えない
- 同じメールアドレスなら、既存のメール登録アカウントにまとめる（登録前の Lambda で紐づける）

## この手順で使う値

| 名前 | 値 |
|---|---|
| Cognito のドメイン | `kotozute.auth.ap-northeast-1.amazoncognito.com`（2026-11-12 時点で空いていた。作るのは `template.yaml` 側） |
| 戻り先 URL（両方で使う） | `https://kotozute.auth.ap-northeast-1.amazoncognito.com/oauth2/idpresponse` |
| Team ID | `5V6S74G5N9` |
| アプリの Bundle ID | `dev.kotozute.app` |

> 戻り先 URL は Apple・Google から Cognito へ結果を返す先。アプリへ返す先（`kotozute://` など）は
> Cognito の設定なので、ここでは入れない。

---

## 1. Apple

[Certificates, Identifiers & Profiles](https://developer.apple.com/account/resources/identifiers/list) で行う。

### 1-1. アプリで Sign in with Apple を有効にする
1. **Identifiers** → `dev.kotozute.app` を開く
2. **Capabilities** の一覧で **Sign In with Apple** にチェック（設定は「Enable as a primary App ID」のまま）
3. **Save**。「プロファイルが無効になる」と出たら **Confirm**（次のビルドで自動で作り直される）

### 1-2. Services ID を作る（Web 経由のログイン用）
Cognito は Web の流れで Apple にログインするので、App ID とは別に「Services ID」が要る。

1. **Identifiers** の **＋** → **Services IDs** → Continue
2. 入力して Register
   - Description: `ことづて ログイン`
   - Identifier: `dev.kotozute.app.signin`
3. 作った `dev.kotozute.app.signin` を開き、**Sign In with Apple** にチェック → **Configure**
   - Primary App ID: `dev.kotozute.app`
   - Domains and Subdomains: `kotozute.auth.ap-northeast-1.amazoncognito.com`
   - Return URLs: `https://kotozute.auth.ap-northeast-1.amazoncognito.com/oauth2/idpresponse`
4. **Next** → **Done** → **Continue** → **Save**

### 1-3. 鍵を作る
1. **Keys** の **＋**
2. Key Name: `ことづて Sign in with Apple`、**Sign in with Apple** にチェック → **Configure** → Primary App ID に `dev.kotozute.app` → Save
3. **Continue** → **Register** → **Download**
   - **ダウンロードは1回しかできない。** なくしたら作り直し
   - 置き場所: `~/.config/kotozute-secrets/AuthKey_<キーID>.p8`（フォルダを作り、`chmod 600`）
   - **リポジトリ・チャット・メモには貼らない。** Claude にも中身は見せない
4. 画面に出る **Key ID**（10文字）を控える

---

## 2. Google

[Google Cloud Console](https://console.cloud.google.com/) で行う。

### 2-1. プロジェクトを作る
1. 上部のプロジェクト選択 → **新しいプロジェクト** → 名前 `kotozute` → 作成
2. 作ったプロジェクトに切り替える

### 2-2. 同意画面（ログイン時に出る「ことづて がアクセスを求めています」の画面）
**Google Auth Platform**（「API とサービス」→「OAuth 同意画面」）で:

1. **ブランディング**
   - アプリ名: `ことづて`
   - ユーザーサポートメール: `support@kotozute.app`（選べなければ自分の Gmail）
   - アプリのホームページ: `https://kotozute.app`
   - プライバシーポリシー: `https://kotozute.app/privacy`
   - 利用規約: `https://kotozute.app/terms`
   - 承認済みドメイン: `kotozute.app` と `amazoncognito.com`
   - デベロッパーの連絡先: 自分のメール
   - ロゴは**入れない**（入れると Google の審査が要る）
2. **対象**: 「外部」
3. **データアクセス**: スコープに `openid`、`.../auth/userinfo.email`、`.../auth/userinfo.profile` の3つ
   （どれも審査不要の範囲）
4. **対象** の画面で **アプリを公開**（本番環境へ）
   - 「テスト」のままだと、登録したテストユーザーしかログインできない

### 2-3. OAuth クライアントを作る
**クライアント** → **クライアントを作成**

- アプリケーションの種類: **ウェブ アプリケーション**（iOS ではない。Cognito が Web の流れで使うため）
- 名前: `Cognito`
- 承認済みの JavaScript 生成元: `https://kotozute.auth.ap-northeast-1.amazoncognito.com`
- 承認済みのリダイレクト URI: `https://kotozute.auth.ap-northeast-1.amazoncognito.com/oauth2/idpresponse`

作成すると **クライアント ID** と **クライアント シークレット** が出る。

- クライアント ID は秘密ではない（控えておく）
- **クライアント シークレットは秘密。** JSON をダウンロードして `~/.config/kotozute-secrets/google-oauth-client.json`
  に置き、`chmod 600`。チャットには貼らない

---

## 3. 終わったら Claude に伝えるもの

秘密でない値だけ:

- Apple の **Key ID**
- Google の **クライアント ID**（`….apps.googleusercontent.com`）
- 鍵とシークレットを上の場所に置いたこと

秘密の値（Apple の .p8、Google のシークレット）は、AWS Secrets Manager に入れる。
入れるためのスクリプトは Claude が用意し、**実行は自分で行う**（Claude は中身を見ない）。
CloudFormation は `{{resolve:secretsmanager:...}}` でそこから読むので、`template.yaml` や
`samconfig.toml` にも秘密は入らない。

## 次（Claude 側）
- `template.yaml`：Cognito ドメイン、Apple / Google の連携、アプリの戻り先（`kotozute://`）、
  登録前に同じメールのアカウントへまとめる Lambda
- アプリ：サインイン・登録画面に Apple / Google のボタン、初めての人の表示名入力
- アカウント削除で Apple の連携を解除する対応、プライバシーポリシーの追記
