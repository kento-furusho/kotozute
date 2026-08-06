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
	"github.com/google/uuid"
)

const tableName = "kotozute"

// 認証導入までは「招待を作る人」をk01に固定する。Cognito導入後、トークンから取得する形に差し替える。
const fixedUserID = "k01"

const inviteTTL = 7 * 24 * time.Hour

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

type inviteResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expiresAt"`
}

func handler(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	userOut, err := dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("USER#%s", fixedUserID)},
			"SK": &types.AttributeValueMemberS{Value: "META"},
		},
	})
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to fetch user"), nil
	}
	if len(userOut.Item) == 0 {
		return errorResponse(http.StatusBadRequest, "user must register before creating an invite"), nil
	}
	if v, ok := userOut.Item["pairId"].(*types.AttributeValueMemberS); ok && v.Value != "" {
		return errorResponse(http.StatusConflict, "user is already paired"), nil
	}

	token := uuid.NewString()
	now := time.Now().UTC()
	expiresAt := now.Add(inviteTTL)

	item := map[string]types.AttributeValue{
		"PK":            &types.AttributeValueMemberS{Value: fmt.Sprintf("INVITE#%s", token)},
		"SK":            &types.AttributeValueMemberS{Value: "META"},
		"inviterUserId": &types.AttributeValueMemberS{Value: fixedUserID},
		"status":        &types.AttributeValueMemberS{Value: "pending"},
		"ttl":           &types.AttributeValueMemberN{Value: strconv.FormatInt(expiresAt.Unix(), 10)},
		"createdAt":     &types.AttributeValueMemberS{Value: now.Format(time.RFC3339)},
	}

	if _, err := dynamoClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(tableName),
		Item:      item,
	}); err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to create invite"), nil
	}

	resBody, err := json.Marshal(inviteResponse{
		Token:     token,
		ExpiresAt: expiresAt.Format(time.RFC3339),
	})
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to build response"), nil
	}

	return events.APIGatewayProxyResponse{
		StatusCode: http.StatusCreated,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(resBody),
	}, nil
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
