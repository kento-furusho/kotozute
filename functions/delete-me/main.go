package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	cognito "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
)

const tableName = "kotozute"

// BatchWriteItem が1回で受け取れる上限。
const batchWriteLimit = 25

var dynamoClient *dynamodb.Client
var cognitoClient *cognito.Client
var snsClient *sns.Client
var userPoolID string

// userIDFromRequest はAPI Gatewayが検証済みのCognitoトークンから、ユーザーの一意なID（sub）を取り出す。
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
	cognitoClient = cognito.NewFromConfig(cfg)
	snsClient = sns.NewFromConfig(cfg)
	userPoolID = os.Getenv("USER_POOL_ID")
}

// handler はアカウントを消す。App Store の要件（アカウントを作れるなら消せること）。
//
// 消すのは「自分のもの」だけ:
//   - 自分が書いたことづて
//   - 自分の USER META
//   - 自分の Cognito ユーザー
//
// **相手が書いたことづては消さない。** ことづては書いた人の下（USER#<書いた人>）に
// あり、相手のものは相手のデータ。別れ話のたびに相手の記録が巻き添えで消えるのは筋が違う。
//
// ペアは解消する。相手はペア未成立に戻り、また誰かを招待できる。
//
// 順番は「ペア解消 → ことづて → USER META → Cognito」。
// Cognito を最後にするのは、途中で失敗しても本人がやり直せる状態を残すため。
func handler(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	userID, ok := userIDFromRequest(request)
	if !ok {
		return errorResponse(http.StatusUnauthorized, "unauthorized"), nil
	}

	meta, err := getUserMeta(ctx, userID)
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to fetch user"), nil
	}

	// USER META が無くても Cognito のユーザーは残っていることがある
	// （登録の途中で止まった人）。消す対象が無いだけなので、そのまま先へ進める。
	if pairID := stringAttr(meta, "pairId"); pairID != "" {
		if err := dissolvePair(ctx, pairID, userID); err != nil {
			return errorResponse(http.StatusInternalServerError, "failed to dissolve pair"), nil
		}
	}

	if err := deleteKotozute(ctx, userID); err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to delete kotozute"), nil
	}

	// 端末の登録も落とす。残すと消えたユーザー宛に配信を試み続けることになる。
	// 失敗しても本体の削除は止めない。
	if arn := stringAttr(meta, "snsEndpointArn"); arn != "" {
		if _, err := snsClient.DeleteEndpoint(ctx, &sns.DeleteEndpointInput{
			EndpointArn: aws.String(arn),
		}); err != nil {
			log.Printf("failed to delete SNS endpoint for user %s: %v", userID, err)
		}
	}

	if len(meta) > 0 {
		if _, err := dynamoClient.DeleteItem(ctx, &dynamodb.DeleteItemInput{
			TableName: aws.String(tableName),
			Key:       userKey(userID),
		}); err != nil {
			return errorResponse(http.StatusInternalServerError, "failed to delete user"), nil
		}
	}

	// 最後に Cognito。ここまで来たらデータは消えているので、
	// 失敗しても本人がもう一度呼べばやり直せる。
	if userPoolID != "" {
		if _, err := cognitoClient.AdminDeleteUser(ctx, &cognito.AdminDeleteUserInput{
			UserPoolId: aws.String(userPoolID),
			Username:   aws.String(userID),
		}); err != nil {
			log.Printf("failed to delete cognito user %s: %v", userID, err)
			return errorResponse(http.StatusInternalServerError, "failed to delete account"), nil
		}
	}

	return events.APIGatewayProxyResponse{StatusCode: http.StatusNoContent}, nil
}

// dissolvePair はペアを解消する。相手の pairId を外し、PAIR の META を消す。
//
// 相手のことづてには触らない。相手の手元には残り、また誰かを招待できる状態に戻る。
func dissolvePair(ctx context.Context, pairID, userID string) error {
	out, err := dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key:       pairKey(pairID),
	})
	if err != nil {
		return err
	}
	// ペアが既に無いなら、自分の pairId が残っているだけ。消す対象は無い。
	if len(out.Item) == 0 {
		return nil
	}

	items := []types.TransactWriteItem{
		{Delete: &types.Delete{TableName: aws.String(tableName), Key: pairKey(pairID)}},
	}
	if partner := partnerUserID(out.Item, userID); partner != "" {
		items = append(items, types.TransactWriteItem{
			Update: &types.Update{
				TableName: aws.String(tableName),
				Key:       userKey(partner),
				// 解消の時刻を刻む。次のペアの区切りの下限になる。
				// これが無いと、この関係で書いた分が相手の次の相手に流れる。
				UpdateExpression: aws.String("REMOVE pairId SET lastPairEndedAt = :endedAt"),
				// 相手が先に消えている場合もある。そのときは何もしない。
				ConditionExpression: aws.String("attribute_exists(PK)"),
				ExpressionAttributeValues: map[string]types.AttributeValue{
					":endedAt": &types.AttributeValueMemberS{
						Value: time.Now().UTC().Format(time.RFC3339),
					},
				},
			},
		})
	}

	_, err = dynamoClient.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: items,
	})
	return err
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

// deleteKotozute は自分のことづてを全部消す。
//
// 何枚あるか分からないので、引きながら 25 件ずつ消す。
// 本文は要らないのでキーだけ取る。
func deleteKotozute(ctx context.Context, userID string) error {
	var startKey map[string]types.AttributeValue
	for {
		out, err := dynamoClient.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(tableName),
			KeyConditionExpression: aws.String("PK = :pk AND begins_with(SK, :prefix)"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk":     &types.AttributeValueMemberS{Value: userPK(userID)},
				":prefix": &types.AttributeValueMemberS{Value: "KOTOZUTE#"},
			},
			ProjectionExpression: aws.String("PK, SK"),
			ExclusiveStartKey:    startKey,
		})
		if err != nil {
			return err
		}
		if err := deleteInBatches(ctx, out.Items); err != nil {
			return err
		}
		if len(out.LastEvaluatedKey) == 0 {
			return nil
		}
		startKey = out.LastEvaluatedKey
	}
}

func deleteInBatches(ctx context.Context, items []map[string]types.AttributeValue) error {
	for start := 0; start < len(items); start += batchWriteLimit {
		end := min(start+batchWriteLimit, len(items))

		requests := make([]types.WriteRequest, 0, end-start)
		for _, item := range items[start:end] {
			requests = append(requests, types.WriteRequest{
				DeleteRequest: &types.DeleteRequest{Key: item},
			})
		}

		out, err := dynamoClient.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{
			RequestItems: map[string][]types.WriteRequest{tableName: requests},
		})
		if err != nil {
			return err
		}
		// 捌ききれなかった分は返ってくる。放っておくと消し残る。
		for len(out.UnprocessedItems[tableName]) > 0 {
			out, err = dynamoClient.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{
				RequestItems: out.UnprocessedItems,
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func getUserMeta(ctx context.Context, userID string) (map[string]types.AttributeValue, error) {
	out, err := dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key:       userKey(userID),
	})
	if err != nil {
		return nil, err
	}
	return out.Item, nil
}

func stringAttr(item map[string]types.AttributeValue, key string) string {
	if v, ok := item[key].(*types.AttributeValueMemberS); ok {
		return strings.TrimSpace(v.Value)
	}
	return ""
}

func userPK(userID string) string { return fmt.Sprintf("USER#%s", userID) }

func userKey(userID string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"PK": &types.AttributeValueMemberS{Value: userPK(userID)},
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
