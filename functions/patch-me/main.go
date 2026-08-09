package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const tableName = "kotozute"

var dynamoClient *dynamodb.Client

// userIDFromRequest はAPI Gatewayが検証済みのCognitoトークンから、ユーザーの一意なID（sub）を取り出す。
// トークンが無い/壊れている場合は ok=false を返し、呼び出し側が401にする。
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

// ポインタ型にすることで「JSONにキーが無い（nil）」と「空文字を送ってきた」を区別する。
// nil のフィールドは更新対象にしない。
type patchMeRequest struct {
	DisplayName *string `json:"displayName"`
	ApnsToken   *string `json:"apnsToken"`
}

// buildUpdateExpression は、nil でないフィールドだけを SET する UpdateExpression と
// その値マップを組み立てる純粋関数。更新対象が1つも無ければ ok=false を返す。
func buildUpdateExpression(req patchMeRequest) (string, map[string]types.AttributeValue, bool) {
	setClauses := make([]string, 0, 2)
	values := map[string]types.AttributeValue{}
	if req.DisplayName != nil {
		setClauses = append(setClauses, "displayName = :displayName")
		values[":displayName"] = &types.AttributeValueMemberS{Value: *req.DisplayName}
	}
	if req.ApnsToken != nil {
		setClauses = append(setClauses, "apnsToken = :apnsToken")
		values[":apnsToken"] = &types.AttributeValueMemberS{Value: *req.ApnsToken}
	}
	if len(setClauses) == 0 {
		return "", nil, false
	}
	return "SET " + strings.Join(setClauses, ", "), values, true
}

func handler(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	userID, ok := userIDFromRequest(request)
	if !ok {
		return errorResponse(http.StatusUnauthorized, "unauthorized"), nil
	}

	var req patchMeRequest
	if err := json.Unmarshal([]byte(request.Body), &req); err != nil {
		return errorResponse(http.StatusBadRequest, "invalid request body"), nil
	}

	// 送られてきた項目だけを SET する UpdateExpression を組み立てる。
	updateExpression, values, ok := buildUpdateExpression(req)
	if !ok {
		return errorResponse(http.StatusBadRequest, "nothing to update"), nil
	}

	_, err := dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("USER#%s", userID)},
			"SK": &types.AttributeValueMemberS{Value: "META"},
		},
		UpdateExpression:          aws.String(updateExpression),
		ConditionExpression:       aws.String("attribute_exists(PK)"), // 存在するユーザーだけ更新（無ければ404）
		ExpressionAttributeValues: values,
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return errorResponse(http.StatusNotFound, "user not registered"), nil
		}
		return errorResponse(http.StatusInternalServerError, "failed to update user"), nil
	}

	// 更新後の値を返す（送られた項目のみ反映）。
	res := map[string]string{"userId": userID}
	if req.DisplayName != nil {
		res["displayName"] = *req.DisplayName
	}
	if req.ApnsToken != nil {
		res["apnsToken"] = *req.ApnsToken
	}
	resBody, err := json.Marshal(res)
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to build response"), nil
	}

	return events.APIGatewayProxyResponse{
		StatusCode: http.StatusOK,
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
