#!/bin/bash
# DynamoDB Local にメインテーブル kotozute（PK/SK）をダミー認証情報で作成する。
# GSI①（曜日）・GSI②（メール）は未定義。それらを使う機能を実装するときにここへ追記する。
set -euo pipefail

AWS_ACCESS_KEY_ID=local AWS_SECRET_ACCESS_KEY=local AWS_REGION=ap-northeast-1 \
aws dynamodb create-table \
  --table-name kotozute \
  --attribute-definitions AttributeName=PK,AttributeType=S AttributeName=SK,AttributeType=S \
  --key-schema AttributeName=PK,KeyType=HASH AttributeName=SK,KeyType=RANGE \
  --billing-mode PAY_PER_REQUEST \
  --endpoint-url http://localhost:8000
