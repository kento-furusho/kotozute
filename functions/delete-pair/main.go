package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const tableName = "kotozute"

var dynamoClient *dynamodb.Client

func userIDFromRequest(request events.APIGatewayProxyRequest) (string, bool) {
	claims, ok := request.RequestContext.Authorizer["claims"].(map[string]interface{})
	if !ok {
		return "", false
	}
	sub, ok := claims["sub"].(string)
	if !ok || sub == "" {
		return "", false
	}
	return sub, true
}

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

// handler はペアを解消する。アカウントは残る。
//
// ことづては**どちらの分も消さない**。書いた人の下にあり、書いた人のもの。
// ただし解消の時刻を両者に記録し、次のペアはそこから始める（lastPairEndedAt）。
// これが無いと、前の相手に向けて書いた分が次の相手の最初のオープンで全部見えてしまう。
//
// 解消は片方から実行できる。相手の同意を待つ仕組みは作らない。
// 関係を続けるかは本人が決めることで、相手の承認が要るとやめられなくなる。
func handler(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	userID, ok := userIDFromRequest(request)
	if !ok {
		return errorResponse(http.StatusUnauthorized, "unauthorized"), nil
	}

	meta, err := getItem(ctx, userKey(userID))
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to fetch user"), nil
	}
	pairID := stringAttr(meta, "pairId")
	if pairID == "" {
		return errorResponse(http.StatusNotFound, "no pair"), nil
	}

	pair, err := getItem(ctx, pairKey(pairID))
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to fetch pair"), nil
	}

	endedAt := time.Now().UTC().Format(time.RFC3339)
	items := []types.TransactWriteItem{
		{Delete: &types.Delete{TableName: aws.String(tableName), Key: pairKey(pairID)}},
		unlink(userID, endedAt),
	}
	if partner := partnerUserID(pair, userID); partner != "" {
		items = append(items, unlink(partner, endedAt))
	}

	if _, err := dynamoClient.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: items,
	}); err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to dissolve pair"), nil
	}

	return events.APIGatewayProxyResponse{StatusCode: http.StatusNoContent}, nil
}

// unlink はペアの紐付けを外し、解消の時刻を刻む。
//
// lastPairEndedAt は次のペアの区切りの下限になる。ここを刻まないと、
// 前の関係で書いた分が次の相手に流れる。
func unlink(userID, endedAt string) types.TransactWriteItem {
	return types.TransactWriteItem{
		Update: &types.Update{
			TableName:        aws.String(tableName),
			Key:              userKey(userID),
			UpdateExpression: aws.String("REMOVE pairId SET lastPairEndedAt = :endedAt"),
			// 相手が先に消えている場合もある。そのときは何もしない。
			ConditionExpression: aws.String("attribute_exists(PK)"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":endedAt": &types.AttributeValueMemberS{Value: endedAt},
			},
		},
	}
}

// partnerUserID は PAIR META から自分ではないほうのメンバーを選ぶ純粋関数。
func partnerUserID(item map[string]types.AttributeValue, userID string) string {
	for _, key := range []string{"member1", "member2"} {
		if v, ok := item[key].(*types.AttributeValueMemberS); ok && v.Value != userID {
			return v.Value
		}
	}
	return ""
}

func getItem(ctx context.Context, key map[string]types.AttributeValue) (map[string]types.AttributeValue, error) {
	out, err := dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key:       key,
	})
	if err != nil {
		return nil, err
	}
	return out.Item, nil
}

func stringAttr(item map[string]types.AttributeValue, key string) string {
	if v, ok := item[key].(*types.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}

func userKey(userID string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("USER#%s", userID)},
		"SK": &types.AttributeValueMemberS{Value: "META"},
	}
}

func pairKey(pairID string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("PAIR#%s", pairID)},
		"SK": &types.AttributeValueMemberS{Value: "META"},
	}
}

func errorResponse(statusCode int, message string) events.APIGatewayProxyResponse {
	body, _ := json.Marshal(map[string]string{"error": message})
	return events.APIGatewayProxyResponse{
		StatusCode: statusCode,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(body),
	}
}

func main() {
	lambda.Start(handler)
}
