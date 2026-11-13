#!/bin/bash
# Apple / Google ログインの秘密の値を AWS Secrets Manager に入れる（作る or 上書き）。
#
#   ~/Developer/kotozute/scripts/admin/put-social-login-secrets.sh
#
# 読むもの（どちらか片方だけでもよい。無いほうは前の値を残す）:
#   ~/.config/kotozute-secrets/AuthKey_<キーID>.p8          Apple の Sign in with Apple 鍵
#   ~/.config/kotozute-secrets/google-oauth-client.json     Google の OAuth クライアント（ウェブ）
#
# 入れる先: シークレット `kotozute/social-login`（JSON）
#   { "applePrivateKey": "...", "googleClientSecret": "..." }
# template.yaml は {{resolve:secretsmanager:kotozute/social-login:SecretString:...}} で読む。
#
# 値は画面にもログにも出さない。一時ファイルは自分だけが読める場所に作り、終わったら消す。
set -euo pipefail

REGION="ap-northeast-1"
SECRET_ID="kotozute/social-login"
DIR="$HOME/.config/kotozute-secrets"

# ローカル開発用のダミーキーが残っていると本番に届かないので外す。
if [ "${AWS_ACCESS_KEY_ID:-}" = "local" ]; then
  unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN
fi
export AWS_REGION="$REGION" AWS_DEFAULT_REGION="$REGION"

tmp=$(mktemp -d)
chmod 700 "$tmp"
trap 'rm -rf "$tmp"' EXIT

# 前の値（あれば）を土台にする。片方ずつ入れても、もう片方が消えないように。
if aws secretsmanager get-secret-value --secret-id "$SECRET_ID" \
     --query SecretString --output text > "$tmp/current.json" 2>/dev/null; then
  exists=true
else
  echo '{}' > "$tmp/current.json"
  exists=false
fi

python3 - "$DIR" "$tmp/current.json" "$tmp/new.json" <<'EOF'
import glob, json, os, sys
d, cur_path, out_path = sys.argv[1:4]
data = json.load(open(cur_path))
keys = sorted(glob.glob(os.path.join(d, "AuthKey_*.p8")))
if len(keys) > 1:
    sys.exit(f"Apple の鍵が {len(keys)} 個あります。使うもの1つだけを残してください。")
if keys:
    data["applePrivateKey"] = open(keys[0]).read().strip()
    data["appleKeyId"] = os.path.basename(keys[0])[len("AuthKey_"):-len(".p8")]
    print(f"Apple の鍵を入れます（キーID {data['appleKeyId']}）")
g = os.path.join(d, "google-oauth-client.json")
if os.path.exists(g):
    web = json.load(open(g)).get("web") or {}
    if not web.get("client_secret"):
        sys.exit("google-oauth-client.json に web.client_secret がありません（種類が「ウェブ アプリケーション」か確認）")
    data["googleClientSecret"] = web["client_secret"]
    data["googleClientId"] = web.get("client_id", "")
    print(f"Google のシークレットを入れます（クライアントID {data['googleClientId']}）")
if not keys and not os.path.exists(g):
    sys.exit(f"{d} に Apple の鍵も Google の JSON もありません")
fd = os.open(out_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
with os.fdopen(fd, "w") as f:
    json.dump(data, f)
EOF

if $exists; then
  aws secretsmanager put-secret-value --secret-id "$SECRET_ID" \
    --secret-string "file://$tmp/new.json" > /dev/null
  echo "更新しました: $SECRET_ID"
else
  aws secretsmanager create-secret --name "$SECRET_ID" \
    --description "ことづて Apple / Google ログインの秘密の値" \
    --secret-string "file://$tmp/new.json" > /dev/null
  echo "作りました: $SECRET_ID"
fi
