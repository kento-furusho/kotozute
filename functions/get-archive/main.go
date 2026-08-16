package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const tableName = "kotozute"

const (
	defaultLimit = 10
	maxLimit     = 50
)

var dynamoClient *dynamodb.Client

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

type archiveQuery struct {
	Whose  string // "me" | "partner"
	Year   int
	Month  int
	Limit  int
	Cursor string
}

type archiveItem struct {
	ID        string `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
}

type archiveResponse struct {
	Items      []archiveItem `json:"items"`
	NextCursor *string       `json:"nextCursor"`
}

type pairMembers struct {
	Member1 string
	Member2 string
}

func handler(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	userID, ok := userIDFromRequest(request)
	if !ok {
		return errorResponse(http.StatusUnauthorized, "unauthorized"), nil
	}

	q, errMsg := parseArchiveQuery(request.QueryStringParameters)
	if errMsg != "" {
		return errorResponse(http.StatusBadRequest, errMsg), nil
	}

	startKey, err := decodeCursor(q.Cursor)
	if err != nil {
		return errorResponse(http.StatusBadRequest, "invalid cursor"), nil
	}

	pairID, err := getUserPairID(ctx, userID)
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to fetch user"), nil
	}
	if pairID == "" {
		return errorResponse(http.StatusNotFound, "no pair"), nil
	}

	// 誰のことづてを見るか。partner の場合は PAIR META から相手を解決する。
	targetUser := userID
	partnerOnly := false
	cycleStartedAt := ""
	if q.Whose == "partner" {
		members, err := getPairMembers(ctx, pairID)
		if err != nil {
			return errorResponse(http.StatusInternalServerError, "failed to fetch pair"), nil
		}
		targetUser = partnerUserID(members, userID)
		partnerOnly = true

		// オープン済みかどうかの判定に、書くサイクルの開始時刻が要る。
		cycleStartedAt, err = getCycleStartedAt(ctx, pairID)
		if err != nil {
			return errorResponse(http.StatusInternalServerError, "failed to fetch pair"), nil
		}
	}

	// SK は KOTOZUTE#<書いた人>#<ISO日時>#<id>。ISO日時は "2026-08-..." なので、
	// 年月プレフィックス "YYYY-MM" で begins_with すれば、その月のことづてに絞れる。
	skPrefix := fmt.Sprintf("KOTOZUTE#%s#%04d-%02d", targetUser, q.Year, q.Month)

	input := &dynamodb.QueryInput{
		TableName:              aws.String(tableName),
		KeyConditionExpression: aws.String("PK = :pk AND begins_with(SK, :prefix)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":     &types.AttributeValueMemberS{Value: fmt.Sprintf("PAIR#%s", pairID)},
			":prefix": &types.AttributeValueMemberS{Value: skPrefix},
		},
		Limit: aws.Int32(int32(q.Limit)),
	}
	if startKey != nil {
		input.ExclusiveStartKey = startKey
	}
	// 相手の過去分はオープン済みのみ見せる（過去ロックの緩和）。
	// オープン済み ＝ 書くサイクルの開始より前に書かれたもの。
	// まだ一度もオープンしていない場合（cycleStartedAt が空）は、見せられるものが無い。
	if partnerOnly {
		input.FilterExpression = aws.String("createdAt < :cycleStartedAt")
		input.ExpressionAttributeValues[":cycleStartedAt"] = &types.AttributeValueMemberS{Value: cycleStartedAt}
	}

	out, err := dynamoClient.Query(ctx, input)
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to query archive"), nil
	}

	items := make([]archiveItem, 0, len(out.Items))
	for _, av := range out.Items {
		item, ok := parseArchiveItem(av)
		if !ok {
			continue
		}
		items = append(items, item)
	}

	res := archiveResponse{Items: items, NextCursor: encodeCursor(out.LastEvaluatedKey)}
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

// parseArchiveQuery はクエリパラメータを検証しつつ archiveQuery に変換する純粋関数。
// 不正があれば errMsg（400用）を返す。
func parseArchiveQuery(params map[string]string) (archiveQuery, string) {
	q := archiveQuery{Whose: "me", Limit: defaultLimit}

	if w, ok := params["whose"]; ok && w != "" {
		if w != "me" && w != "partner" {
			return archiveQuery{}, "whose must be 'me' or 'partner'"
		}
		q.Whose = w
	}

	year, err := strconv.Atoi(params["year"])
	if err != nil || year < 2000 || year > 2100 {
		return archiveQuery{}, "year is required and must be valid"
	}
	q.Year = year

	month, err := strconv.Atoi(params["month"])
	if err != nil || month < 1 || month > 12 {
		return archiveQuery{}, "month is required and must be between 1 and 12"
	}
	q.Month = month

	if l, ok := params["limit"]; ok && l != "" {
		limit, err := strconv.Atoi(l)
		if err != nil || limit < 1 {
			return archiveQuery{}, "limit must be a positive integer"
		}
		if limit > maxLimit {
			limit = maxLimit
		}
		q.Limit = limit
	}

	q.Cursor = params["cursor"]
	return q, ""
}

// encodeCursor は DynamoDB の LastEvaluatedKey を不透明な cursor 文字列に変換する。
// ベーステーブルのキーは PK/SK（どちらも文字列）なので、その文字列を JSON→Base64 で包む。
// 続きが無い（空）の場合は nil を返し、レスポンスの nextCursor は null になる。
func encodeCursor(lek map[string]types.AttributeValue) *string {
	if len(lek) == 0 {
		return nil
	}
	m := map[string]string{}
	for k, v := range lek {
		if s, ok := v.(*types.AttributeValueMemberS); ok {
			m[k] = s.Value
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	encoded := base64.StdEncoding.EncodeToString(b)
	return &encoded
}

// decodeCursor は cursor 文字列を ExclusiveStartKey（DynamoDB のキー）に戻す。
// 空なら nil（先頭から）。壊れていれば error。
func decodeCursor(cursor string) (map[string]types.AttributeValue, error) {
	if cursor == "" {
		return nil, nil
	}
	b, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	out := map[string]types.AttributeValue{}
	for k, v := range m {
		out[k] = &types.AttributeValueMemberS{Value: v}
	}
	return out, nil
}

func parseArchiveItem(av map[string]types.AttributeValue) (archiveItem, bool) {
	sk, ok := av["SK"].(*types.AttributeValueMemberS)
	if !ok {
		return archiveItem{}, false
	}
	parts := strings.SplitN(sk.Value, "#", 4)
	if len(parts) != 4 {
		return archiveItem{}, false
	}
	item := archiveItem{ID: parts[3]}
	if v, ok := av["body"].(*types.AttributeValueMemberS); ok {
		item.Body = v.Value
	}
	if v, ok := av["createdAt"].(*types.AttributeValueMemberS); ok {
		item.CreatedAt = v.Value
	}
	return item, true
}

// partnerUserID は「自分じゃない方のメンバー」を返す純粋関数。
func partnerUserID(members pairMembers, myUserID string) string {
	if members.Member1 == myUserID {
		return members.Member2
	}
	return members.Member1
}

// getCycleStartedAt は PAIR META から「書くサイクルの開始時刻」を読む。
// これより前に書かれたことづてがオープン済み。
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

func getPairMembers(ctx context.Context, pairID string) (pairMembers, error) {
	out, err := dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("PAIR#%s", pairID)},
			"SK": &types.AttributeValueMemberS{Value: "META"},
		},
	})
	if err != nil {
		return pairMembers{}, err
	}
	members := pairMembers{}
	if v, ok := out.Item["member1"].(*types.AttributeValueMemberS); ok {
		members.Member1 = v.Value
	}
	if v, ok := out.Item["member2"].(*types.AttributeValueMemberS); ok {
		members.Member2 = v.Value
	}
	return members, nil
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
