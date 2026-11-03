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
// 門番（Cognito Authorizer）が認証を通したあと、トークンの中身（claims）を
// request.RequestContext.Authorizer["claims"] に詰めてくれる。ここから "sub" を掘り出す。
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

type meResponse struct {
	UserID      string `json:"userId"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email,omitempty"`
	PairID      string `json:"pairId,omitempty"`
	PairStatus  string `json:"pairStatus"`
	NextOpenAt  string `json:"nextOpenAt,omitempty"`
}

type pairMeta struct {
	OpenDayOfWeek int
	OpenTime      string
	Timezone      string
	// 前回オープンした時刻。周期が2週以上のとき、次のオープンを決めるのに要る。
	CycleStartedAt string
	// オープンの周期（週）。属性が無ければ毎週（1）。
	OpenIntervalWeeks int
}

func handler(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	// トークンから「自分が誰か」を得る。ここが取れない＝正規の認証を通っていないので401。
	userID, ok := userIDFromRequest(request)
	if !ok {
		return errorResponse(http.StatusUnauthorized, "unauthorized"), nil
	}

	userOut, err := dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("USER#%s", userID)},
			"SK": &types.AttributeValueMemberS{Value: "META"},
		},
	})
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to fetch user"), nil
	}
	if len(userOut.Item) == 0 {
		return errorResponse(http.StatusNotFound, "user not registered"), nil
	}

	res := meResponse{
		UserID:     userID,
		PairStatus: "none",
	}
	if v, ok := userOut.Item["displayName"].(*types.AttributeValueMemberS); ok {
		res.DisplayName = v.Value
	}
	if v, ok := userOut.Item["email"].(*types.AttributeValueMemberS); ok {
		res.Email = v.Value
	}

	pairID := ""
	if v, ok := userOut.Item["pairId"].(*types.AttributeValueMemberS); ok {
		pairID = v.Value
	}

	if pairID != "" {
		pairOut, err := dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
			TableName: aws.String(tableName),
			Key: map[string]types.AttributeValue{
				"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("PAIR#%s", pairID)},
				"SK": &types.AttributeValueMemberS{Value: "META"},
			},
		})
		if err != nil {
			return errorResponse(http.StatusInternalServerError, "failed to fetch pair"), nil
		}
		if len(pairOut.Item) > 0 {
			res.PairID = pairID
			res.PairStatus = "paired"

			meta := buildPairMetaFromItem(pairOut.Item)
			nextOpenAt, err := computeNextOpenAt(meta, time.Now())
			if err != nil {
				return errorResponse(http.StatusInternalServerError, "failed to compute next open date"), nil
			}
			res.NextOpenAt = nextOpenAt
		}
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

func buildPairMetaFromItem(item map[string]types.AttributeValue) pairMeta {
	meta := pairMeta{
		OpenDayOfWeek: 5, // 金曜(仮)。PATCH /pair実装後は実データを使う。
		OpenTime:      "20:00",
		Timezone:      "Asia/Tokyo",
	}
	if v, ok := item["openDayOfWeek"].(*types.AttributeValueMemberN); ok {
		if n, err := strconv.Atoi(v.Value); err == nil {
			meta.OpenDayOfWeek = n
		}
	}
	if v, ok := item["openTime"].(*types.AttributeValueMemberS); ok {
		meta.OpenTime = v.Value
	}
	if v, ok := item["timezone"].(*types.AttributeValueMemberS); ok {
		meta.Timezone = v.Value
	}
	if v, ok := item["cycleStartedAt"].(*types.AttributeValueMemberS); ok {
		meta.CycleStartedAt = v.Value
	}
	if v, ok := item["openIntervalWeeks"].(*types.AttributeValueMemberN); ok {
		if n, err := strconv.Atoi(v.Value); err == nil {
			meta.OpenIntervalWeeks = n
		}
	}
	return meta
}

// computeNextOpenAt は次回オープン日時を算出する（周期を含む）。
func computeNextOpenAt(meta pairMeta, now time.Time) (string, error) {
	next, err := nextOpenMoment(meta.OpenDayOfWeek, meta.OpenTime, meta.Timezone, meta.CycleStartedAt, meta.OpenIntervalWeeks, now)
	if err != nil {
		return "", err
	}
	return next.Format("2006-01-02T15:04"), nil
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
