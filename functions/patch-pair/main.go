package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
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

// nil のフィールドは更新対象にしない（未指定と区別するためポインタ型）。
type patchPairRequest struct {
	OpenDayOfWeek *int    `json:"openDayOfWeek"`
	OpenTime      *string `json:"openTime"`
	Timezone      *string `json:"timezone"`
	// オープンの周期（週）。1〜4。
	OpenIntervalWeeks *int `json:"openIntervalWeeks"`
}

func handler(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	userID, ok := userIDFromRequest(request)
	if !ok {
		return errorResponse(http.StatusUnauthorized, "unauthorized"), nil
	}

	var req patchPairRequest
	if err := json.Unmarshal([]byte(request.Body), &req); err != nil {
		return errorResponse(http.StatusBadRequest, "invalid request body"), nil
	}

	// 送られた項目を検証しつつ UpdateExpression を組み立てる。errMsg があれば 400。
	updateExpression, values, errMsg := buildPairUpdate(req)
	if errMsg != "" {
		return errorResponse(http.StatusBadRequest, errMsg), nil
	}

	// ことづての pairID と同様、トークンのユーザーを起点にペアを辿る。
	pairID, err := getUserPairID(ctx, userID)
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to fetch user"), nil
	}
	if pairID == "" {
		return errorResponse(http.StatusNotFound, "no pair"), nil
	}

	_, err = dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("PAIR#%s", pairID)},
			"SK": &types.AttributeValueMemberS{Value: "META"},
		},
		UpdateExpression:          aws.String(updateExpression),
		ConditionExpression:       aws.String("attribute_exists(PK)"),
		ExpressionAttributeValues: values,
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return errorResponse(http.StatusNotFound, "no pair"), nil
		}
		return errorResponse(http.StatusInternalServerError, "failed to update pair"), nil
	}

	res := map[string]interface{}{"pairId": pairID}
	if req.OpenDayOfWeek != nil {
		res["openDayOfWeek"] = *req.OpenDayOfWeek
	}
	if req.OpenTime != nil {
		res["openTime"] = *req.OpenTime
	}
	if req.Timezone != nil {
		res["timezone"] = *req.Timezone
	}
	if req.OpenIntervalWeeks != nil {
		res["openIntervalWeeks"] = *req.OpenIntervalWeeks
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

// オープンの周期として選べる範囲（週）。
const (
	minIntervalWeeks = 1
	maxIntervalWeeks = 4
)

// buildPairUpdate は送られた項目を検証しつつ UpdateExpression と値マップを組み立てる純粋関数。
// 不正な値があれば errMsg（400用メッセージ）を返す。更新対象がゼロでも errMsg を返す。
func buildPairUpdate(req patchPairRequest) (string, map[string]types.AttributeValue, string) {
	setClauses := make([]string, 0, 3)
	values := map[string]types.AttributeValue{}

	if req.OpenDayOfWeek != nil {
		if *req.OpenDayOfWeek < 0 || *req.OpenDayOfWeek > 6 {
			return "", nil, "openDayOfWeek must be between 0 and 6"
		}
		setClauses = append(setClauses, "openDayOfWeek = :openDayOfWeek")
		values[":openDayOfWeek"] = &types.AttributeValueMemberN{Value: strconv.Itoa(*req.OpenDayOfWeek)}
	}
	if req.OpenTime != nil {
		if !isValidHHMM(*req.OpenTime) {
			return "", nil, "openTime must be in HH:MM format"
		}
		setClauses = append(setClauses, "openTime = :openTime")
		values[":openTime"] = &types.AttributeValueMemberS{Value: *req.OpenTime}
	}
	if req.Timezone != nil {
		if _, err := time.LoadLocation(*req.Timezone); err != nil {
			return "", nil, "timezone is not a valid IANA timezone"
		}
		setClauses = append(setClauses, "timezone = :timezone")
		values[":timezone"] = &types.AttributeValueMemberS{Value: *req.Timezone}
	}

	if req.OpenIntervalWeeks != nil {
		// 週単位で1〜4。長すぎると「書いたのに何週も読めない」になり、続かない。
		if *req.OpenIntervalWeeks < minIntervalWeeks || *req.OpenIntervalWeeks > maxIntervalWeeks {
			return "", nil, "openIntervalWeeks must be between 1 and 4"
		}
		setClauses = append(setClauses, "openIntervalWeeks = :openIntervalWeeks")
		values[":openIntervalWeeks"] = &types.AttributeValueMemberN{Value: strconv.Itoa(*req.OpenIntervalWeeks)}
	}

	if len(setClauses) == 0 {
		return "", nil, "nothing to update"
	}
	return "SET " + strings.Join(setClauses, ", "), values, ""
}

// isValidHHMM は "HH:MM"（00:00〜23:59）かどうかを判定する。
func isValidHHMM(s string) bool {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return false
	}
	hour, err := strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > 23 {
		return false
	}
	minute, err := strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > 59 {
		return false
	}
	// "9:5" のような桁抜けは許さず、常に2桁表記に限定する。
	return len(parts[0]) == 2 && len(parts[1]) == 2
}

// getUserPairID は USER#<userID>/META から pairId を読む。ペア未成立なら空文字を返す。
func getUserPairID(ctx context.Context, userID string) (string, error) {
	out, err := dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("USER#%s", userID)},
			"SK": &types.AttributeValueMemberS{Value: "META"},
		},
	})
	if err != nil {
		return "", err
	}
	if v, ok := out.Item["pairId"].(*types.AttributeValueMemberS); ok {
		return v.Value, nil
	}
	return "", nil
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
