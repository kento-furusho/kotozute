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
	// 付箋の色。6桁の16進数。省略すると色は触らない。
	// "none" を渡すと色を外す（並び順で決める見た目に戻す）。
	Color string `json:"color"`
}

type kotozuteResponse struct {
	ID    string `json:"id"`
	Body  string `json:"body"`
	Color string `json:"color,omitempty"`
}

// foundKotozute は検索で見つけたことづての、更新に必要な情報。
type foundKotozute struct {
	SK        string
	CreatedAt string
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
	color, clearColor, ok := parseColorUpdate(req.Color)
	if !ok {
		return errorResponse(http.StatusBadRequest, "color must be a 6-digit hex"), nil
	}

	pairID, err := getUserPairID(ctx, userID)
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to fetch user"), nil
	}
	if pairID == "" {
		return errorResponse(http.StatusNotFound, "kotozute not found"), nil
	}

	// URLのidだけでは完全なSKを組み立てられないため、自分のことづてを検索してidが一致する1件を探す。
	// 自分のパーティションしか引かないので、他人のことづては構造的に見つからない。
	found, err := findMyKotozute(ctx, pairID, userID, kotozuteID)
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to fetch kotozute"), nil
	}
	if found == nil {
		return errorResponse(http.StatusNotFound, "kotozute not found"), nil
	}
	// 編集できるのは「書くサイクル」のものだけ。オープン済みは確定扱いで編集不可。
	cycleStartedAt, err := getCycleStartedAt(ctx, pairID)
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to fetch pair"), nil
	}
	if found.CreatedAt < cycleStartedAt {
		return errorResponse(http.StatusConflict, "opened kotozute cannot be edited"), nil
	}

	// 本文のみ更新。検索と更新の間にバッチが区切りを進める競合に備え、
	// ペアの cycleStartedAt が変わっていないことを条件に付けたトランザクションで書く。
	// （オープン済みかどうかはことづて単体では判定できず、ペアの区切りとの関係で決まるため）
	_, err = dynamoClient.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: []types.TransactWriteItem{
			{
				ConditionCheck: &types.ConditionCheck{
					TableName: aws.String(tableName),
					Key: map[string]types.AttributeValue{
						"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("PAIR#%s", pairID)},
						"SK": &types.AttributeValueMemberS{Value: "META"},
					},
					ConditionExpression: aws.String("cycleStartedAt = :cycle"),
					ExpressionAttributeValues: map[string]types.AttributeValue{
						":cycle": &types.AttributeValueMemberS{Value: cycleStartedAt},
					},
				},
			},
			{
				Update: &types.Update{
					TableName: aws.String(tableName),
					Key: map[string]types.AttributeValue{
						"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("USER#%s", userID)},
						"SK": &types.AttributeValueMemberS{Value: found.SK},
					},
					UpdateExpression: aws.String(colorUpdateExpression(color, clearColor)),
					ExpressionAttributeNames: map[string]string{
						"#body": "body",
					},
					ExpressionAttributeValues: colorUpdateValues(req.Body, color),
				},
			},
		},
	})
	if err != nil {
		var canceled *types.TransactionCanceledException
		if errors.As(err, &canceled) {
			// 直前にオープンされた等で条件を満たさなくなった。
			return errorResponse(http.StatusConflict, "opened kotozute cannot be edited"), nil
		}
		return errorResponse(http.StatusInternalServerError, "failed to update kotozute"), nil
	}

	resBody, err := json.Marshal(kotozuteResponse{ID: kotozuteID, Body: req.Body, Color: color})
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
			":pk":     &types.AttributeValueMemberS{Value: fmt.Sprintf("USER#%s", userID)},
			":prefix": &types.AttributeValueMemberS{Value: "KOTOZUTE#"},
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
		createdAt := ""
		if v, ok := item["createdAt"].(*types.AttributeValueMemberS); ok {
			createdAt = v.Value
		}
		return &foundKotozute{SK: sk.Value, CreatedAt: createdAt}, nil
	}
	return nil, nil
}

// kotozuteIDFromSK は "KOTOZUTE#<日時>#<id>" の末尾idを取り出す。
// 書いた人はパーティション側にあるのでSKには含まれない。形式が違えば空文字を返す。
func kotozuteIDFromSK(sk string) string {
	parts := strings.SplitN(sk, "#", 3)
	if len(parts) != 3 {
		return ""
	}
	return parts[2]
}

// getCycleStartedAt は PAIR META から「書くサイクルの開始時刻」を読む。
// これより後に書かれたことづてがまだオープンされていないもの。
func getCycleStartedAt(ctx context.Context, pairID string) (string, error) {
	out, err := dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("PAIR#%s", pairID)},
			"SK": &types.AttributeValueMemberS{Value: "META"},
		},
	})
	if err != nil {
		return "", err
	}
	if v, ok := out.Item["cycleStartedAt"].(*types.AttributeValueMemberS); ok {
		return v.Value, nil
	}
	return "", nil
}

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

// parseColorUpdate は色の指定を読む。
//
//	""     … 触らない（色は今のまま）
//	"none" … 外す（並び順で決める見た目に戻す）
//	16進6桁 … その色にする
//
// 「触らない」と「外す」を分けるのは、書き直しのたびに色が消えるのを防ぐため。
// 省略＝消す にすると、本文だけ直したいときに色まで失われる。
func parseColorUpdate(value string) (color string, clear bool, ok bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", false, true
	}
	if trimmed == "none" {
		return "", true, true
	}
	trimmed = strings.TrimPrefix(trimmed, "#")
	if len(trimmed) != 6 {
		return "", false, false
	}
	for _, r := range trimmed {
		isDigit := r >= '0' && r <= '9'
		isHex := (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		if !isDigit && !isHex {
			return "", false, false
		}
	}
	return strings.ToUpper(trimmed), false, true
}

func colorUpdateExpression(color string, clear bool) string {
	switch {
	case clear:
		return "SET #body = :body REMOVE color"
	case color != "":
		return "SET #body = :body, color = :color"
	default:
		return "SET #body = :body"
	}
}

func colorUpdateValues(body, color string) map[string]types.AttributeValue {
	values := map[string]types.AttributeValue{
		":body": &types.AttributeValueMemberS{Value: body},
	}
	if color != "" {
		values[":color"] = &types.AttributeValueMemberS{Value: color}
	}
	return values
}

func main() {
	lambda.Start(handler)
}
