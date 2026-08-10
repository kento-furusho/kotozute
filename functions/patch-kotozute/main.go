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

type patchKotozuteRequest struct {
	Body string `json:"body"`
}

type kotozuteResponse struct {
	ID   string `json:"id"`
	Body string `json:"body"`
}

// foundKotozute は検索で見つけたことづての、更新に必要な情報。
type foundKotozute struct {
	SK     string
	Opened bool
}

func handler(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	userID, ok := userIDFromRequest(request)
	if !ok {
		return errorResponse(http.StatusUnauthorized, "unauthorized"), nil
	}

	kotozuteID := request.PathParameters["id"]
	if kotozuteID == "" {
		return errorResponse(http.StatusBadRequest, "id is required"), nil
	}

	var req patchKotozuteRequest
	if err := json.Unmarshal([]byte(request.Body), &req); err != nil {
		return errorResponse(http.StatusBadRequest, "invalid request body"), nil
	}
	if req.Body == "" {
		return errorResponse(http.StatusBadRequest, "body is required"), nil
	}

	pairID, err := getUserPairID(ctx, userID)
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to fetch user"), nil
	}
	if pairID == "" {
		return errorResponse(http.StatusNotFound, "kotozute not found"), nil
	}

	// URLのidだけでは完全なSKを組み立てられないため、自分のことづてを検索してidが一致する1件を探す。
	// KOTOZUTE#<自分>#... しか検索しないので、他人のことづては構造的に見つからない。
	found, err := findMyKotozute(ctx, pairID, userID, kotozuteID)
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to fetch kotozute"), nil
	}
	if found == nil {
		return errorResponse(http.StatusNotFound, "kotozute not found"), nil
	}
	// 編集はオープン前のみ。オープン済みは確定扱いで編集不可。
	if found.Opened {
		return errorResponse(http.StatusConflict, "opened kotozute cannot be edited"), nil
	}

	// 本文のみ更新。検索と更新の間にバッチがオープンする競合に備え、opened=false を条件にする。
	_, err = dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("PAIR#%s", pairID)},
			"SK": &types.AttributeValueMemberS{Value: found.SK},
		},
		UpdateExpression:    aws.String("SET #body = :body"),
		ConditionExpression: aws.String("#opened = :false"),
		ExpressionAttributeNames: map[string]string{
			"#body":   "body",
			"#opened": "opened",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":body":  &types.AttributeValueMemberS{Value: req.Body},
			":false": &types.AttributeValueMemberBOOL{Value: false},
		},
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			// 直前にオープンされた等で条件を満たさなくなった。
			return errorResponse(http.StatusConflict, "opened kotozute cannot be edited"), nil
		}
		return errorResponse(http.StatusInternalServerError, "failed to update kotozute"), nil
	}

	resBody, err := json.Marshal(kotozuteResponse{ID: kotozuteID, Body: req.Body})
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to build response"), nil
	}

	return events.APIGatewayProxyResponse{
		StatusCode: http.StatusOK,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(resBody),
	}, nil
}

// findMyKotozute は自分の書いたことづての中から、末尾idが一致する1件を探す。
// 見つからなければ nil を返す。
func findMyKotozute(ctx context.Context, pairID, userID, kotozuteID string) (*foundKotozute, error) {
	out, err := dynamoClient.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(tableName),
		KeyConditionExpression: aws.String("PK = :pk AND begins_with(SK, :prefix)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":     &types.AttributeValueMemberS{Value: fmt.Sprintf("PAIR#%s", pairID)},
			":prefix": &types.AttributeValueMemberS{Value: fmt.Sprintf("KOTOZUTE#%s#", userID)},
		},
	})
	if err != nil {
		return nil, err
	}

	for _, item := range out.Items {
		sk, ok := item["SK"].(*types.AttributeValueMemberS)
		if !ok {
			continue
		}
		if kotozuteIDFromSK(sk.Value) != kotozuteID {
			continue
		}
		opened := false
		if v, ok := item["opened"].(*types.AttributeValueMemberBOOL); ok {
			opened = v.Value
		}
		return &foundKotozute{SK: sk.Value, Opened: opened}, nil
	}
	return nil, nil
}

// kotozuteIDFromSK は "KOTOZUTE#<書いた人>#<日時>#<id>" の末尾idを取り出す。
// 形式が違えば空文字を返す。
func kotozuteIDFromSK(sk string) string {
	parts := strings.SplitN(sk, "#", 4)
	if len(parts) != 4 {
		return ""
	}
	return parts[3]
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
