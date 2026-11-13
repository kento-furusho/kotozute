#!/bin/bash
# 本番のアカウントを、アプリの「アカウントを削除」と同じ処理で消す。
#
#   ./scripts/admin/delete-users.sh              消す予定の一覧を出すだけ（何も消さない）
#   ./scripts/admin/delete-users.sh --execute    確認のうえ、実際に消す
#
# 消す相手:  ~/.config/kotozute-admin/delete-list.txt（1行1メール、# 以降は無視）
# 残す相手:  ~/.config/kotozute-admin/keep-list.txt（delete-list に入っていても消さない）
# どちらもリポジトリには置かない（メールアドレスが入るため）。
#
# 消し方は delete-me の Lambda を直接呼ぶ。API Gateway を通さず、Cognito の
# claims（sub）を載せたイベントを渡す。アプリから消したときと同じく
#   ペア解消 → ことづて・ご意見 → USER META → 端末の登録 → Cognito
# の順で消える。相手が書いたことづては相手のものなので残る。
#
# **取り返しがつかない。** 実行前に一覧を必ず見ること。
set -euo pipefail

REGION="ap-northeast-1"
USER_POOL_ID="ap-northeast-1_4mx0ifrEZ"
STACK="kotozute"
CONF="$HOME/.config/kotozute-admin"
DELETE_LIST="$CONF/delete-list.txt"
KEEP_LIST="$CONF/keep-list.txt"

# ローカル開発用のダミーキーが残っていると本番に届かないので外す。
if [ "${AWS_ACCESS_KEY_ID:-}" = "local" ]; then
  unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN
fi
export AWS_REGION="$REGION" AWS_DEFAULT_REGION="$REGION"

execute=false
[ "${1:-}" = "--execute" ] && execute=true

read_list() { [ -f "$1" ] && sed -e 's/#.*//' -e 's/[[:space:]]//g' "$1" | grep -v '^$' || true; }

keep=$(read_list "$KEEP_LIST")
targets=()
echo "消す予定:"
while IFS= read -r email; do
  [ -z "$email" ] && continue
  if grep -qixF "$email" <<<"$keep"; then
    echo "  [残す一覧にあるので飛ばす] $email"
    continue
  fi
  sub=$(aws cognito-idp list-users --user-pool-id "$USER_POOL_ID" \
          --filter "email = \"$email\"" --query 'Users[0].Attributes[?Name==`sub`]|[0].Value' --output text)
  if [ "$sub" = "None" ] || [ -z "$sub" ]; then
    echo "  [見つからない] $email"
    continue
  fi
  echo "  $email  ($sub)"
  targets+=("$sub|$email")
done < <(read_list "$DELETE_LIST")

echo "合計 ${#targets[@]} 件"
if ! $execute; then
  echo "（一覧だけ。消すときは --execute を付けて実行）"
  exit 0
fi
[ ${#targets[@]} -eq 0 ] && exit 0

read -r -p "本当に ${#targets[@]} 件を消しますか？取り消せません。消すなら delete と入力: " answer
[ "$answer" = "delete" ] || { echo "やめました。"; exit 1; }

fn=$(aws cloudformation describe-stack-resource --stack-name "$STACK" \
       --logical-resource-id DeleteMeFunction --query 'StackResourceDetail.PhysicalResourceId' --output text)

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
failed=0
for t in "${targets[@]}"; do
  sub="${t%%|*}"; email="${t#*|}"
  # skipAppleRevoke: Apple の連携の取り消しには本人の Apple ログインが要り、ここからはできない。
  # 付けないと Apple で入った人は 409 で止まる（アプリには本人確認を頼む合図）。
  payload=$(printf '{"requestContext":{"authorizer":{"claims":{"sub":"%s"}}},"body":"{\\"skipAppleRevoke\\":true}"}' "$sub")
  aws lambda invoke --function-name "$fn" --cli-binary-format raw-in-base64-out \
    --payload "$payload" "$tmp/out.json" >/dev/null
  status=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("statusCode"))' "$tmp/out.json")
  if [ "$status" = "204" ]; then
    echo "  消しました: $email"
  else
    echo "  失敗: $email → $(cat "$tmp/out.json")"
    failed=$((failed + 1))
  fi
done
echo "完了（失敗 $failed 件）"
