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

	resBody, err := json.Marshal(inviteInfoResponse{
		InviterDisplayName: displayName.Value,
		Status:             status.Value,
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
