package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const tableName = "kotozute"

// 認証・ペア機能導入までは「招待を受ける人」「成立するペア」を固定値にする。
// Cognito導入後、トークンから取得する形に差し替える。
const (
	fixedAcceptingUserID = "k02"
	fixedPairID          = "p01"
)

// PAIR META新規作成時のデフォルト値。PATCH /pair実装後、成立後に変更できるようにする。
const (
	defaultOpenDayOfWeek = 5 // 金曜(仮)
	defaultOpenTime      = "20:00"
	defaultTimezone      = "Asia/Tokyo"
)

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

type acceptRequest struct {
	DisplayName string `json:"displayName"`
}

type acceptResponse struct {
	PairID string `json:"pairId"`
}

func handler(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	token := request.PathParameters["token"]
	if token == "" {
		return errorResponse(http.StatusBadRequest, "token is required"), nil
	}

	var req acceptRequest
	if request.Body != "" {
		if err := json.Unmarshal([]byte(request.Body), &req); err != nil {
			return errorResponse(http.StatusBadRequest, "invalid request body"), nil
		}
	}

	inviteOut, err := dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("INVITE#%s", token)},
			"SK": &types.AttributeValueMemberS{Value: "META"},
		},
	})
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to fetch invite"), nil
	}
	if len(inviteOut.Item) == 0 {
		return errorResponse(http.StatusNotFound, "invite not found"), nil
	}
	if isInviteExpired(inviteOut.Item, time.Now()) {
		return errorResponse(http.StatusNotFound, "invite not found"), nil
	}

	status, ok := inviteOut.Item["status"].(*types.AttributeValueMemberS)
	if !ok {
		return errorResponse(http.StatusInternalServerError, "invite is malformed"), nil
	}
	if status.Value != "pending" {
		return errorResponse(http.StatusConflict, "invite has already been used"), nil
	}

	inviterUserID, ok := inviteOut.Item["inviterUserId"].(*types.AttributeValueMemberS)
	if !ok {
		return errorResponse(http.StatusInternalServerError, "invite is malformed"), nil
	}
	if inviterUserID.Value == fixedAcceptingUserID {
		return errorResponse(http.StatusBadRequest, "cannot accept your own invite"), nil
	}

	// 招待を受ける側がPOST /usersで既に登録済みなら、その属性（email/apnsToken）を引き継ぐ。
	// まだ登録されていなければ、受け入れと同時にこのacceptで初期登録する。
	acceptingUserOut, err := dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("USER#%s", fixedAcceptingUserID)},
			"SK": &types.AttributeValueMemberS{Value: "META"},
		},
	})
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to fetch accepting user"), nil
	}

	displayName := req.DisplayName
	email := ""
	apnsToken := ""
	if len(acceptingUserOut.Item) > 0 {
		if displayName == "" {
			if v, ok := acceptingUserOut.Item["displayName"].(*types.AttributeValueMemberS); ok {
				displayName = v.Value
			}
		}
		if v, ok := acceptingUserOut.Item["email"].(*types.AttributeValueMemberS); ok {
			email = v.Value
		}
		if v, ok := acceptingUserOut.Item["apnsToken"].(*types.AttributeValueMemberS); ok {
			apnsToken = v.Value
		}
	}
	if displayName == "" {
		displayName = "パートナー"
	}

	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339)

	_, err = dynamoClient.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: []types.TransactWriteItem{
			{
				Update: &types.Update{
					TableName: aws.String(tableName),
					Key: map[string]types.AttributeValue{
						"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("INVITE#%s", token)},
						"SK": &types.AttributeValueMemberS{Value: "META"},
					},
					UpdateExpression:    aws.String("SET #status = :accepted"),
					ConditionExpression: aws.String("#status = :pending AND #ttl > :now"),
					ExpressionAttributeNames: map[string]string{
						"#status": "status",
						"#ttl":    "ttl",
					},
					ExpressionAttributeValues: map[string]types.AttributeValue{
						":accepted": &types.AttributeValueMemberS{Value: "accepted"},
						":pending":  &types.AttributeValueMemberS{Value: "pending"},
						":now":      &types.AttributeValueMemberN{Value: strconv.FormatInt(now.Unix(), 10)},
					},
				},
			},
			{
				Put: &types.Put{
					TableName: aws.String(tableName),
					Item: map[string]types.AttributeValue{
						"PK":            &types.AttributeValueMemberS{Value: fmt.Sprintf("PAIR#%s", fixedPairID)},
						"SK":            &types.AttributeValueMemberS{Value: "META"},
						"member1":       &types.AttributeValueMemberS{Value: inviterUserID.Value},
						"member2":       &types.AttributeValueMemberS{Value: fixedAcceptingUserID},
						"openDayOfWeek": &types.AttributeValueMemberN{Value: strconv.Itoa(defaultOpenDayOfWeek)},
						"openTime":      &types.AttributeValueMemberS{Value: defaultOpenTime},
						"timezone":      &types.AttributeValueMemberS{Value: defaultTimezone},
						"status":        &types.AttributeValueMemberS{Value: "active"},
						"lastOpenedAt":  &types.AttributeValueMemberS{Value: nowStr},
					},
					ConditionExpression: aws.String("attribute_not_exists(PK)"),
				},
			},
			{
				Update: &types.Update{
					TableName: aws.String(tableName),
					Key: map[string]types.AttributeValue{
						"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("USER#%s", inviterUserID.Value)},
						"SK": &types.AttributeValueMemberS{Value: "META"},
					},
					UpdateExpression:    aws.String("SET pairId = :pairId"),
					ConditionExpression: aws.String("attribute_exists(PK) AND attribute_not_exists(pairId)"),
					ExpressionAttributeValues: map[string]types.AttributeValue{
						":pairId": &types.AttributeValueMemberS{Value: fixedPairID},
					},
				},
			},
			{
				Put: &types.Put{
					TableName: aws.String(tableName),
					Item: map[string]types.AttributeValue{
						"PK":          &types.AttributeValueMemberS{Value: fmt.Sprintf("USER#%s", fixedAcceptingUserID)},
						"SK":          &types.AttributeValueMemberS{Value: "META"},
						"displayName": &types.AttributeValueMemberS{Value: displayName},
						"email":       &types.AttributeValueMemberS{Value: email},
						"apnsToken":   &types.AttributeValueMemberS{Value: apnsToken},
						"pairId":      &types.AttributeValueMemberS{Value: fixedPairID},
					},
					ConditionExpression: aws.String("attribute_not_exists(pairId)"),
				},
			},
		},
	})
	if err != nil {
		var cancelErr *types.TransactionCanceledException
		if errors.As(err, &cancelErr) {
			return errorResponse(http.StatusConflict, "invite may have expired, already been used, or the pair already exists"), nil
		}
		return errorResponse(http.StatusInternalServerError, "failed to accept invite"), nil
	}

	resBody, err := json.Marshal(acceptResponse{PairID: fixedPairID})
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to build response"), nil
	}

	return events.APIGatewayProxyResponse{
		StatusCode: http.StatusOK,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(resBody),
	}, nil
}

// isInviteExpired はINVITE METAのttl（unix epoch秒）と現在時刻を比較する。
// ttl属性が無い/不正な場合は安全側に倒して「期限切れ」として扱う。
func isInviteExpired(item map[string]types.AttributeValue, now time.Time) bool {
	v, ok := item["ttl"].(*types.AttributeValueMemberN)
	if !ok {
		return true
	}
	ttl, err := strconv.ParseInt(v.Value, 10, 64)
	if err != nil {
		return true
	}
	return now.Unix() > ttl
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
