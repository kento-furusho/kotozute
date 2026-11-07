// admin-feedback は届いたご意見を新しい順に返す。
//
// admin-stats と同じく API Gateway にはつながない。手元の
// scripts/admin/dashboard.py が IAM の権限で `aws lambda invoke` する。
//
// 返すのは SK が FEEDBACK# の項目だけ。絞り込みは DynamoDB 側（FilterExpression）で
// 行うので、ことづての本文がこの Lambda に渡ってくることはない。
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const tableName = "kotozute"

var dynamoClient *dynamodb.Client

func init() {
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		panic(fmt.Sprintf("failed to load AWS config: %v", err))
	}
	endpoint := os.Getenv("DYNAMODB_ENDPOINT")
	dynamoClient = dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
	})
}

type feedback struct {
	// 送った人。返信したいときは、これで Cognito からメールアドレスを引く。
	UserID     string `json:"userId"`
	Body       string `json:"body"`
	AppVersion string `json:"appVersion,omitempty"`
	CreatedAt  string `json:"createdAt"`
}

type response struct {
	Items []feedback `json:"items"`
}

func handler(ctx context.Context) (response, error) {
	var items []feedback
	var startKey map[string]types.AttributeValue
	for {
		out, err := dynamoClient.Scan(ctx, &dynamodb.ScanInput{
			TableName:        aws.String(tableName),
			FilterExpression: aws.String("begins_with(SK, :prefix)"),
			// "body" はことづてにもある属性名だが、FEEDBACK# 以外の項目は
			// DynamoDB 側で落ちてから返ってくる。
			ProjectionExpression: aws.String("PK, SK, body, appVersion, createdAt"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":prefix": &types.AttributeValueMemberS{Value: "FEEDBACK#"},
			},
			ExclusiveStartKey: startKey,
		})
		if err != nil {
			return response{}, err
		}
		for _, item := range out.Items {
			items = append(items, toFeedback(item))
		}
		if len(out.LastEvaluatedKey) == 0 {
			break
		}
		startKey = out.LastEvaluatedKey
	}
	sortNewestFirst(items)
	if items == nil {
		items = []feedback{}
	}
	return response{Items: items}, nil
}

func toFeedback(item map[string]types.AttributeValue) feedback {
	return feedback{
		UserID:     strings.TrimPrefix(stringAttr(item, "PK"), "USER#"),
		Body:       stringAttr(item, "body"),
		AppVersion: stringAttr(item, "appVersion"),
		CreatedAt:  stringAttr(item, "createdAt"),
	}
}

// sortNewestFirst は新しい順に並べる。createdAt は UTC の RFC3339 で揃えて
// 保存しているので、文字列の比較で日時の順になる。
func sortNewestFirst(items []feedback) {
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].CreatedAt > items[j].CreatedAt
	})
}

func stringAttr(item map[string]types.AttributeValue, name string) string {
	if v, ok := item[name].(*types.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}

func main() {
	lambda.Start(handler)
}
