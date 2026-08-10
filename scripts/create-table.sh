#!/bin/bash
# DynamoDB Local にメインテーブル kotozute（PK/SK）をダミー認証情報で作成する。
# GSI①（曜日）を定義済み。GSI②（メール）はログイン機能実装時にここへ追記する。
#
# GSI①（GSI1）: openDayOfWeek(N) を PK にした索引。週一バッチが「今日オープンすべきペア」を検索する。
#   openDayOfWeek 属性を持つのは PAIR META だけなので、この索引には自然とペアだけが載る（スパース索引）。
#   Projection=ALL にして、バッチが member1/member2/lastOpenedAt などを追加読み込みなしで得られるようにする。
#
# 注意：既にテーブルが存在する環境で GSI を後から足す場合は create ではなく
#       `aws dynamodb update-table --global-secondary-index-updates '[{"Create":{...}}]'` を使う。
set -euo pipefail

AWS_ACCESS_KEY_ID=local AWS_SECRET_ACCESS_KEY=local AWS_REGION=ap-northeast-1 \
aws dynamodb create-table \
  --table-name kotozute \
  --attribute-definitions \
      AttributeName=PK,AttributeType=S \
      AttributeName=SK,AttributeType=S \
      AttributeName=openDayOfWeek,AttributeType=N \
  --key-schema AttributeName=PK,KeyType=HASH AttributeName=SK,KeyType=RANGE \
  --global-secondary-indexes '[
    {
      "IndexName": "GSI1",
      "KeySchema": [
        {"AttributeName": "openDayOfWeek", "KeyType": "HASH"}
      ],
      "Projection": {"ProjectionType": "ALL"}
    }
  ]' \
  --billing-mode PAY_PER_REQUEST \
  --endpoint-url http://localhost:8000
