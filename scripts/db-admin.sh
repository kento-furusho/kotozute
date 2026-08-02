#!/bin/bash
# DynamoDB Local を GUI で確認する（dynamodb-admin）。
# ローカル操作は必ずダミー認証情報で統一する（本番キーのローテーションでローカルテーブルが迷子になるのを防ぐため）。
set -euo pipefail

AWS_REGION=ap-northeast-1 \
AWS_ACCESS_KEY_ID=local \
AWS_SECRET_ACCESS_KEY=local \
DYNAMO_ENDPOINT=http://localhost:8000 \
dynamodb-admin
