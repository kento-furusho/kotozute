package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
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
	// オープンの周期（週）。1=毎週、2=2週ごと…。
	OpenIntervalWeeks int    `json:"openIntervalWeeks"`
	Timezone          string `json:"timezone"`
	Status            string `json:"status"`
	NextOpenAt        string `json:"nextOpenAt"`
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
	// 前回オープンした時刻と周期。次のオープンを決めるのに要る。
	CycleStartedAt    string
	OpenIntervalWeeks int
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

	nextOpenAt, err := computeNextOpenAt(*meta, time.Now())
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to compute next open date"), nil
	}

	res := pairResponse{
		PairID:            pairID,
		Partner:           partnerInfo{UserID: partnerID, DisplayName: partnerName},
		OpenDayOfWeek:     meta.OpenDayOfWeek,
		OpenTime:          meta.OpenTime,
		OpenIntervalWeeks: intervalOrWeekly(meta.OpenIntervalWeeks),
		Timezone:          meta.Timezone,
		Status:            meta.Status,
		NextOpenAt:        nextOpenAt,
		CycleStarts:       meta.CycleStarts,
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

// sortedUnique は重複を取り除いて古い順に並べる。
// ISO8601 の文字列は辞書順が時刻順と一致するので、文字列のまま並べてよい。
func sortedUnique(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
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
	// 直近2つの区切りは必ず含める。
	// 履歴を持たない古いペアや、履歴の記録を始める前のオープン分を取りこぼさないため
	// （list_append は属性が無いところから始まるので、それ以前の区切りが履歴に入らない）。
	for _, key := range []string{"previousCycleStartedAt", "cycleStartedAt"} {
		if v, ok := item[key].(*types.AttributeValueMemberS); ok {
			meta.CycleStarts = append(meta.CycleStarts, v.Value)
		}
	}
	meta.CycleStarts = sortedUnique(meta.CycleStarts)
	if v, ok := item["cycleStartedAt"].(*types.AttributeValueMemberS); ok {
		meta.CycleStartedAt = v.Value
	}
	if v, ok := item["openIntervalWeeks"].(*types.AttributeValueMemberN); ok {
		if n, err := strconv.Atoi(v.Value); err == nil {
			meta.OpenIntervalWeeks = n
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

// computeNextOpenAt は次回オープン日時を算出する（周期を含む）。
func computeNextOpenAt(meta pairMeta, now time.Time) (string, error) {
	next, err := nextOpenMoment(meta.OpenDayOfWeek, meta.OpenTime, meta.Timezone, meta.CycleStartedAt, meta.OpenIntervalWeeks, now)
	if err != nil {
		return "", err
	}
	return next.Format("2006-01-02T15:04"), nil
}

// intervalOrWeekly は、周期を持たない古いペアを毎週として返す。
func intervalOrWeekly(weeks int) int {
	if weeks < 1 {
		return 1
	}
	return weeks
}

// minOpenGap は、前回のオープンから次のオープンまでに空ける最短の間隔。
// 周期が N 週なら (N − 0.5) 週。毎週なら3.5日、2週ごとなら10.5日。
//
// オープンの曜日は毎週めぐってくるので、周期が2週以上のときはこの間隔で振り分ける
// （2週ごとなら、1週目は7日 < 10.5日で見送り、2週目に14日で開く）。
// 曜日や周期を変えた直後に、前回から1日しか経っていないのに開いてしまうことも防ぐ。
//
// この計算は weekly-open / get-me / get-pair / get-this-week で同じものを使う。
// 片方だけ変えると、アプリに出る「次のオープン」と実際に開く日がずれる。
func minOpenGap(intervalWeeks int) time.Duration {
	if intervalWeeks < 1 {
		intervalWeeks = 1
	}
	return time.Duration(intervalWeeks*7*24-84) * time.Hour
}

// nextOpenMoment は次のオープン日時を返す純粋関数。
// now より後で、かつ前回のオープン（cycleStartedAt）から minOpenGap 以上あいた、最初のオープン曜日・時刻。
func nextOpenMoment(openDayOfWeek int, openTime, timezone, cycleStartedAt string, intervalWeeks int, now time.Time) (time.Time, error) {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.UTC
	}
	nowInLoc := now.In(loc)

	hour, minute, err := parseHHMM(openTime)
	if err != nil {
		return time.Time{}, err
	}

	daysUntil := (openDayOfWeek - int(nowInLoc.Weekday()) + 7) % 7
	candidate := time.Date(nowInLoc.Year(), nowInLoc.Month(), nowInLoc.Day(), hour, minute, 0, 0, loc).
		AddDate(0, 0, daysUntil)
	if !candidate.After(nowInLoc) {
		candidate = candidate.AddDate(0, 0, 7)
	}
	if started, err := time.Parse(time.RFC3339, cycleStartedAt); err == nil {
		// 周期は最大4週なので、5回進めれば必ず届く。壊れた値で回り続けないよう上限を置く。
		for i := 0; i < 5 && candidate.Sub(started) < minOpenGap(intervalWeeks); i++ {
			candidate = candidate.AddDate(0, 0, 7)
		}
	}
	return candidate, nil
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
