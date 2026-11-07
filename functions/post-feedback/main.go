package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/google/uuid"
)

const tableName = "kotozute"

// ご意見の本文の上限（文字数）。長文の要望も入るが、貼り付けで巨大な本文が来るのは防ぐ。
const maxFeedbackLength = 2000

// アプリのバージョン（"1.0.3 (13)" など）の上限。クライアントが送る値なので長さだけ縛る。
const maxAppVersionLength = 32

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

type postFeedbackRequest struct {
	Body       string `json:"body"`
	AppVersion string `json:"appVersion"`
}

// validateFeedback は本文とバージョンを検証し、保存する形に整える。
// 問題があればエラーメッセージを返す（空なら通過）。
func validateFeedback(req postFeedbackRequest) (body, appVersion, errMsg string) {
	body = strings.TrimSpace(req.Body)
	if body == "" {
		return "", "", "body is required"
	}
	if utf8.RuneCountInString(body) > maxFeedbackLength {
		return "", "", fmt.Sprintf("body must be at most %d characters", maxFeedbackLength)
	}
	appVersion = strings.TrimSpace(req.AppVersion)
	if utf8.RuneCountInString(appVersion) > maxAppVersionLength {
		return "", "", "appVersion is too long"
	}
	return body, appVersion, ""
}

func handler(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	// 送った人はトークンから決める。本文に userID を書かせない（なりすましを防ぐ）。
	userID, ok := userIDFromRequest(request)
	if !ok {
		return errorResponse(http.StatusUnauthorized, "unauthorized"), nil
	}

	var req postFeedbackRequest
	if err := json.Unmarshal([]byte(request.Body), &req); err != nil {
		return errorResponse(http.StatusBadRequest, "invalid request body"), nil
	}
	body, appVersion, errMsg := validateFeedback(req)
	if errMsg != "" {
		return errorResponse(http.StatusBadRequest, errMsg), nil
	}

	createdAt := time.Now().UTC().Format(time.RFC3339)
	item := map[string]types.AttributeValue{
		// ご意見も送った人の下に置く。アカウントを削除すると、ことづてと一緒に消える
		// （delete-me が USER#<id> の下を消す）。プライバシーポリシーの約束と揃えるため。
		"PK":        &types.AttributeValueMemberS{Value: fmt.Sprintf("USER#%s", userID)},
		"SK":        &types.AttributeValueMemberS{Value: fmt.Sprintf("FEEDBACK#%s#%s", createdAt, uuid.NewString())},
		"body":      &types.AttributeValueMemberS{Value: body},
		"createdAt": &types.AttributeValueMemberS{Value: createdAt},
	}
	if appVersion != "" {
		item["appVersion"] = &types.AttributeValueMemberS{Value: appVersion}
	}

	if _, err := dynamoClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(tableName),
		Item:      item,
	}); err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to save feedback"), nil
	}

	// 返すものは無い。送れたことだけ伝える。
	return events.APIGatewayProxyResponse{StatusCode: http.StatusNoContent}, nil
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
