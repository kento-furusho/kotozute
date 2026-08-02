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
	"github.com/google/uuid"
)

const tableName = "kotozute"

// 認証導入まではpairID/userIDを固定値にする。Cognito導入後、トークンから取得する形に差し替える。
const fixedPairID = "p01"
const fixedUserID = "k01"

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

type postKotozuteRequest struct {
	Body string `json:"body"`
}

type kotozuteResponse struct {
	ID        string `json:"id"`
	Body      string `json:"body"`
	Opened    bool   `json:"opened"`
	CreatedAt string `json:"createdAt"`
}

func handler(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	var req postKotozuteRequest
	if err := json.Unmarshal([]byte(request.Body), &req); err != nil {
		return errorResponse(http.StatusBadRequest, "invalid request body"), nil
	}
	if req.Body == "" {
		return errorResponse(http.StatusBadRequest, "body is required"), nil
	}

	createdAt := time.Now().UTC().Format(time.RFC3339)
	kotozuteID := uuid.NewString()

	item := map[string]types.AttributeValue{
		"PK":        &types.AttributeValueMemberS{Value: fmt.Sprintf("PAIR#%s", fixedPairID)},
		"SK":        &types.AttributeValueMemberS{Value: fmt.Sprintf("KOTOZUTE#%s#%s#%s", fixedUserID, createdAt, kotozuteID)},
		"body":      &types.AttributeValueMemberS{Value: req.Body},
		"opened":    &types.AttributeValueMemberBOOL{Value: false},
		"createdAt": &types.AttributeValueMemberS{Value: createdAt},
	}

	if _, err := dynamoClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(tableName),
		Item:      item,
	}); err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to save kotozute"), nil
	}

	resBody, err := json.Marshal(kotozuteResponse{
		ID:        kotozuteID,
		Body:      req.Body,
		Opened:    false,
		CreatedAt: createdAt,
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
