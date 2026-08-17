package main

import (
	"context"
	"encoding/json"
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

type partnerInfo struct {
	UserID      string `json:"userId"`
	DisplayName string `json:"displayName"`
}

type pairResponse struct {
	PairID        string      `json:"pairId"`
	Partner       partnerInfo `json:"partner"`
	OpenDayOfWeek int         `json:"openDayOfWeek"`
	OpenTime      string      `json:"openTime"`
	Timezone      string      `json:"timezone"`
	Status        string      `json:"status"`
	NextOpenAt    string      `json:"nextOpenAt"`
	// サイクルの区切り（古い順）。週ごとに遡って読むために返す。
	// 隣り合う2つが1つの週の範囲になる。末尾は「いま書いているサイクル」の開始。
	CycleStarts []string `json:"cycleStarts"`
}

// pairMeta はPAIR METAから読み出す、レスポンス生成に必要な値。
type pairMeta struct {
	Member1       string
	Member2       string
	OpenDayOfWeek int
	OpenTime      string
	Timezone      string
	Status        string
	CycleStarts   []string
}

func handler(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	// 1. トークンから「自分が誰か」を得る。
	userID, ok := userIDFromRequest(request)
	if !ok {
		return errorResponse(http.StatusUnauthorized, "unauthorized"), nil
	}

	// 2. 自分の USER META から pairId を辿る。ペア未成立なら404。
	pairID, err := getUserPairID(ctx, userID)
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to fetch user"), nil
	}
	if pairID == "" {
		return errorResponse(http.StatusNotFound, "no pair"), nil
	}

	// 3. PAIR META を読む。
	meta, err := getPairMeta(ctx, pairID)
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to fetch pair"), nil
	}
	if meta == nil {
		return errorResponse(http.StatusNotFound, "no pair"), nil
	}

	// 4. 相手（自分じゃない方のメンバー）を特定し、その表示名を引く。
	partnerID := partnerUserID(*meta, userID)
	partnerName, err := getUserDisplayName(ctx, partnerID)
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to fetch partner"), nil
	}

	nextOpenAt, err := computeNextOpenAt(meta.OpenDayOfWeek, meta.OpenTime, meta.Timezone, time.Now())
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to compute next open date"), nil
	}

	res := pairResponse{
		PairID:        pairID,
		Partner:       partnerInfo{UserID: partnerID, DisplayName: partnerName},
		OpenDayOfWeek: meta.OpenDayOfWeek,
		OpenTime:      meta.OpenTime,
		Timezone:      meta.Timezone,
		Status:        meta.Status,
		NextOpenAt:    nextOpenAt,
		CycleStarts:   meta.CycleStarts,
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

// getPairMeta は PAIR#<pairID>/META を読む。存在しなければ nil を返す。
func getPairMeta(ctx context.Context, pairID string) (*pairMeta, error) {
	out, err := dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("PAIR#%s", pairID)},
			"SK": &types.AttributeValueMemberS{Value: "META"},
		},
	})
	if err != nil {
		return nil, err
	}
	if len(out.Item) == 0 {
		return nil, nil
	}
	return buildPairMetaFromItem(out.Item), nil
}

// buildPairMetaFromItem は PAIR META の項目から pairMeta を組み立てる。
// オープン設定が未設定の場合はデフォルト値（金曜20:00 Asia/Tokyo）で補う。
func buildPairMetaFromItem(item map[string]types.AttributeValue) *pairMeta {
	meta := &pairMeta{
		OpenDayOfWeek: 5,
		OpenTime:      "20:00",
		Timezone:      "Asia/Tokyo",
	}
	if v, ok := item["member1"].(*types.AttributeValueMemberS); ok {
		meta.Member1 = v.Value
	}
	if v, ok := item["member2"].(*types.AttributeValueMemberS); ok {
		meta.Member2 = v.Value
	}
	if v, ok := item["openDayOfWeek"].(*types.AttributeValueMemberN); ok {
		if n, err := strconv.Atoi(v.Value); err == nil {
			meta.OpenDayOfWeek = n
		}
	}
	if v, ok := item["openTime"].(*types.AttributeValueMemberS); ok {
		meta.OpenTime = v.Value
	}
	if v, ok := item["cycleStarts"].(*types.AttributeValueMemberL); ok {
		for _, entry := range v.Value {
			if s, ok := entry.(*types.AttributeValueMemberS); ok {
				meta.CycleStarts = append(meta.CycleStarts, s.Value)
			}
		}
	}
	// 履歴が無い古いペアは、持っている2つの区切りから組み立てる。
	if len(meta.CycleStarts) == 0 {
		for _, key := range []string{"previousCycleStartedAt", "cycleStartedAt"} {
			if v, ok := item[key].(*types.AttributeValueMemberS); ok {
				meta.CycleStarts = append(meta.CycleStarts, v.Value)
			}
		}
	}
	if v, ok := item["timezone"].(*types.AttributeValueMemberS); ok {
		meta.Timezone = v.Value
	}
	if v, ok := item["status"].(*types.AttributeValueMemberS); ok {
		meta.Status = v.Value
	}
	return meta
}

// partnerUserID は「自分じゃない方のメンバー」を相手として返す純粋関数。
func partnerUserID(meta pairMeta, myUserID string) string {
	if meta.Member1 == myUserID {
		return meta.Member2
	}
	return meta.Member1
}

// getUserDisplayName は USER#<userID>/META から displayName を読む。無ければ空文字。
func getUserDisplayName(ctx context.Context, userID string) (string, error) {
	if userID == "" {
		return "", nil
	}
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
	if v, ok := out.Item["displayName"].(*types.AttributeValueMemberS); ok {
		return v.Value, nil
	}
	return "", nil
}

// computeNextOpenAt は openDayOfWeek/openTime/timezone から直近の次回オープン日時を算出する。
func computeNextOpenAt(openDayOfWeek int, openTime, timezone string, now time.Time) (string, error) {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.UTC
	}
	nowInLoc := now.In(loc)

	hour, minute, err := parseHHMM(openTime)
	if err != nil {
		return "", err
	}

	daysUntil := (openDayOfWeek - int(nowInLoc.Weekday()) + 7) % 7
	candidate := time.Date(nowInLoc.Year(), nowInLoc.Month(), nowInLoc.Day(), hour, minute, 0, 0, loc).
		AddDate(0, 0, daysUntil)
	if !candidate.After(nowInLoc) {
		candidate = candidate.AddDate(0, 0, 7)
	}
	return candidate.Format("2006-01-02T15:04"), nil
}

func parseHHMM(s string) (int, int, error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid time format: %s", s)
	}
	hour, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid hour: %s", parts[0])
	}
	minute, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid minute: %s", parts[1])
	}
	return hour, minute, nil
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
