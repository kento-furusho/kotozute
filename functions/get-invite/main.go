package main

import (
	"context"
	"encoding/json"
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

type inviteInfoResponse struct {
	InviterDisplayName string `json:"inviterDisplayName"`
	Status             string `json:"status"`
	// 招待した人が1枚でも書いているか。真偽値だけで、枚数は返さない。
	// 「相手の気配は存在のみ、枚数は出さない」という決めごとに合わせる。
	InviterHasWritten bool `json:"inviterHasWritten"`
}

func handler(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	token := request.PathParameters["token"]
	if token == "" {
		return errorResponse(http.StatusBadRequest, "token is required"), nil
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

	inviterUserID, ok := inviteOut.Item["inviterUserId"].(*types.AttributeValueMemberS)
	if !ok {
		return errorResponse(http.StatusInternalServerError, "invite is malformed"), nil
	}
	status, ok := inviteOut.Item["status"].(*types.AttributeValueMemberS)
	if !ok {
		return errorResponse(http.StatusInternalServerError, "invite is malformed"), nil
	}

	inviterOut, err := dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("USER#%s", inviterUserID.Value)},
			"SK": &types.AttributeValueMemberS{Value: "META"},
		},
	})
	if err != nil || len(inviterOut.Item) == 0 {
		return errorResponse(http.StatusInternalServerError, "failed to fetch inviter"), nil
	}
	displayName, ok := inviterOut.Item["displayName"].(*types.AttributeValueMemberS)
	if !ok {
		return errorResponse(http.StatusInternalServerError, "inviter is malformed"), nil
	}

	// 招待した人がもう書いているかを見る。招待される側の画面で
	// 「あなた宛のことづてが、もう貼ってある」を出してよいかの判断に使う。
	// 書いていない人の招待でそう出すと嘘になるので、必ずここで確かめる。
	hasWritten, err := hasAnyKotozute(ctx, inviterUserID.Value)
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to fetch inviter kotozute"), nil
	}

	resBody, err := json.Marshal(inviteInfoResponse{
		InviterDisplayName: displayName.Value,
		Status:             status.Value,
		InviterHasWritten:  hasWritten,
	})
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to build response"), nil
	}

	return events.APIGatewayProxyResponse{
		StatusCode: http.StatusOK,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(resBody),
	}, nil
}

// hasAnyKotozute はそのユーザーがことづてを1枚でも持っているかを返す。
//
// 枚数は数えない。1件見つかれば十分なので Limit=1 で打ち切る。
// 数えてしまうと、返さないと決めた情報がログや将来の変更で漏れやすくなる。
//
// SK は KOTOZUTE#<ISO日時>#<id> なので、接頭辞で自分のことづてだけに絞れる。
func hasAnyKotozute(ctx context.Context, userID string) (bool, error) {
	out, err := dynamoClient.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(tableName),
		KeyConditionExpression: aws.String("PK = :pk AND begins_with(SK, :prefix)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":     &types.AttributeValueMemberS{Value: fmt.Sprintf("USER#%s", userID)},
			":prefix": &types.AttributeValueMemberS{Value: "KOTOZUTE#"},
		},
		Limit: aws.Int32(1),
		// 本文は要らない。存在だけ分かればよい。
		ProjectionExpression: aws.String("SK"),
	})
	if err != nil {
		return false, err
	}
	return len(out.Items) > 0, nil
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
