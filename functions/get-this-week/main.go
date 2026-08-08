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

// PAIR META（PAIR#<id>/META）がまだ存在しない場合のデフォルト値。
// lastOpenedAtは遠い過去のsentinelにし、「これまで書いた全部を今週扱い」にする。
// POST /invites/accept 実装後、実データが書き込まれれば自然にそちらへ移行する。
const (
	defaultLastOpenedAt  = "1970-01-01T00:00:00Z"
	defaultOpenDayOfWeek = 5 // time.Weekday: 0=Sun...6=Sat → 5=金曜(仮)
	defaultOpenTime      = "20:00"
	defaultTimezone      = "Asia/Tokyo"
)

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

type kotozuteItem struct {
	ID        string
	Body      string
	Opened    bool
	CreatedAt string
}

type kotozuteResponseItem struct {
	ID        string `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
}

type thisWeekResponse struct {
	MyKotozute        []kotozuteResponseItem `json:"myKotozute"`
	OpenStatus        string                  `json:"openStatus"`
	CanSeePartner     bool                    `json:"canSeePartner"`
	PartnerKotozute   []kotozuteResponseItem  `json:"partnerKotozute,omitempty"`
	PartnerHasWritten bool                    `json:"partnerHasWritten"`
	NextOpenAt        string                  `json:"nextOpenAt"`
}

type pairMeta struct {
	LastOpenedAt  string
	OpenDayOfWeek int
	OpenTime      string
	Timezone      string
	Member1       string // ペアの片方のユーザーID
	Member2       string // ペアのもう片方のユーザーID
}

func handler(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	// 1. トークンから「自分が誰か」を得る。
	userID, ok := userIDFromRequest(request)
	if !ok {
		return errorResponse(http.StatusUnauthorized, "unauthorized"), nil
	}

	// 2. 自分の USER META から pairId を辿る。ペア未成立なら相手はいないので空の状態を返す。
	//    クライアントから pairID を受け取らず、トークンのユーザーを起点に辿ることで他人のペアを覗けない。
	pairID, err := getUserPairID(ctx, userID)
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to fetch user"), nil
	}
	if pairID == "" {
		return okJSON(thisWeekResponse{
			MyKotozute:        []kotozuteResponseItem{},
			OpenStatus:        "before",
			CanSeePartner:     false,
			PartnerHasWritten: false,
			NextOpenAt:        "",
		})
	}

	// 3. PAIR META を読み、相手（自分じゃない方のメンバー）を特定する。
	meta, err := getPairMeta(ctx, pairID)
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to fetch pair"), nil
	}
	partnerID := partnerUserID(meta, userID)

	// 4. 自分・相手それぞれの今週分ことづてを取得する。
	myItems, err := queryKotozuteForUser(ctx, pairID, userID, meta.LastOpenedAt)
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to fetch kotozute"), nil
	}
	partnerItems, err := queryKotozuteForUser(ctx, pairID, partnerID, meta.LastOpenedAt)
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to fetch kotozute"), nil
	}

	nextOpenAt, err := computeNextOpenAt(meta, time.Now())
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to compute next open date"), nil
	}

	// 5. 相互ロック判定（純粋関数）でレスポンスを組み立てる。
	return okJSON(buildThisWeekResponse(myItems, partnerItems, nextOpenAt))
}

// okJSON は thisWeekResponse を 200 のJSONレスポンスに変換する。
func okJSON(res thisWeekResponse) (events.APIGatewayProxyResponse, error) {
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

// buildThisWeekResponse はDynamoDBに依存しない純粋関数。相互ロック判定の核心。
// canSeePartner=falseのときはPartnerKotozuteに一切触れない（本文を詰めるコードパス自体に到達しない）ことで、
// 本文漏洩を構造的に避ける。
func buildThisWeekResponse(myItems, partnerItems []kotozuteItem, nextOpenAt string) thisWeekResponse {
	partnerHasWritten := len(partnerItems) > 0
	partnerOpened := false
	for _, it := range partnerItems {
		if it.Opened {
			partnerOpened = true
			break
		}
	}
	canSeePartner := len(myItems) > 0 && partnerOpened

	openStatus := "before"
	if partnerOpened {
		openStatus = "opened"
	}

	res := thisWeekResponse{
		MyKotozute:        toResponseItems(myItems),
		OpenStatus:        openStatus,
		CanSeePartner:     canSeePartner,
		PartnerHasWritten: partnerHasWritten,
		NextOpenAt:        nextOpenAt,
	}
	if canSeePartner {
		res.PartnerKotozute = toResponseItems(partnerItems)
	}
	return res
}

func toResponseItems(items []kotozuteItem) []kotozuteResponseItem {
	res := make([]kotozuteResponseItem, 0, len(items))
	for _, it := range items {
		res = append(res, kotozuteResponseItem{ID: it.ID, Body: it.Body, CreatedAt: it.CreatedAt})
	}
	return res
}

// queryKotozuteForUser はPK=PAIR#<pairID>、SKが"KOTOZUTE#<userID>#"で始まりlastOpenedAt以降のアイテムを取得する。
// DynamoDBは1つのソートキー属性に対して1条件式しか使えないため、begins_withと範囲条件を
// 同時に満たすべくBETWEENで下限/上限を組み立てる（上限はUnicode上ほぼ最大の文字￿で
// このuserIDプレフィックス内の最大値を保証する定石パターン）。
func queryKotozuteForUser(ctx context.Context, pairID, userID, lastOpenedAt string) ([]kotozuteItem, error) {
	lower := fmt.Sprintf("KOTOZUTE#%s#%s", userID, lastOpenedAt)
	upper := fmt.Sprintf("KOTOZUTE#%s#￿", userID)

	out, err := dynamoClient.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(tableName),
		KeyConditionExpression: aws.String("PK = :pk AND SK BETWEEN :lower AND :upper"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":    &types.AttributeValueMemberS{Value: fmt.Sprintf("PAIR#%s", pairID)},
			":lower": &types.AttributeValueMemberS{Value: lower},
			":upper": &types.AttributeValueMemberS{Value: upper},
		},
	})
	if err != nil {
		return nil, err
	}

	items := make([]kotozuteItem, 0, len(out.Items))
	for _, av := range out.Items {
		item, err := parseKotozuteItem(av)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func parseKotozuteItem(av map[string]types.AttributeValue) (kotozuteItem, error) {
	sk, ok := av["SK"].(*types.AttributeValueMemberS)
	if !ok {
		return kotozuteItem{}, fmt.Errorf("SK missing or invalid type")
	}
	parts := strings.SplitN(sk.Value, "#", 4)
	if len(parts) != 4 {
		return kotozuteItem{}, fmt.Errorf("unexpected SK format: %s", sk.Value)
	}

	body, ok := av["body"].(*types.AttributeValueMemberS)
	if !ok {
		return kotozuteItem{}, fmt.Errorf("body missing or invalid type")
	}
	opened, ok := av["opened"].(*types.AttributeValueMemberBOOL)
	if !ok {
		return kotozuteItem{}, fmt.Errorf("opened missing or invalid type")
	}
	createdAt, ok := av["createdAt"].(*types.AttributeValueMemberS)
	if !ok {
		return kotozuteItem{}, fmt.Errorf("createdAt missing or invalid type")
	}

	return kotozuteItem{
		ID:        parts[3],
		Body:      body.Value,
		Opened:    opened.Value,
		CreatedAt: createdAt.Value,
	}, nil
}

func getPairMeta(ctx context.Context, pairID string) (pairMeta, error) {
	out, err := dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("PAIR#%s", pairID)},
			"SK": &types.AttributeValueMemberS{Value: "META"},
		},
	})
	if err != nil {
		return pairMeta{}, err
	}
	return buildPairMetaFromItem(out.Item), nil
}

// buildPairMetaFromItem はGetItemの結果からpairMetaを組み立てる。
// itemが空（PAIR METAがまだ書き込まれていない）場合はデフォルト値を返す。
func buildPairMetaFromItem(item map[string]types.AttributeValue) pairMeta {
	meta := pairMeta{
		LastOpenedAt:  defaultLastOpenedAt,
		OpenDayOfWeek: defaultOpenDayOfWeek,
		OpenTime:      defaultOpenTime,
		Timezone:      defaultTimezone,
	}
	if len(item) == 0 {
		return meta
	}

	if v, ok := item["lastOpenedAt"].(*types.AttributeValueMemberS); ok {
		meta.LastOpenedAt = v.Value
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
	if v, ok := item["member1"].(*types.AttributeValueMemberS); ok {
		meta.Member1 = v.Value
	}
	if v, ok := item["member2"].(*types.AttributeValueMemberS); ok {
		meta.Member2 = v.Value
	}
	return meta
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

// partnerUserID は「自分じゃない方のメンバー」を相手として返す。
// pairID は自分の USER META から辿って得たものなので、自分は必ずこのペアの一員である。
func partnerUserID(meta pairMeta, myUserID string) string {
	if meta.Member1 == myUserID {
		return meta.Member2
	}
	return meta.Member1
}

// computeNextOpenAt はmetaのopenDayOfWeek/openTime/timezoneから直近の次回オープン日時を算出する。
// バッチ・PATCH /pairが未実装の現段階では、metaがデフォルト値の場合この値は近似値に過ぎない。
func computeNextOpenAt(meta pairMeta, now time.Time) (string, error) {
	loc, err := time.LoadLocation(meta.Timezone)
	if err != nil {
		loc = time.UTC
	}
	nowInLoc := now.In(loc)

	hour, minute, err := parseHHMM(meta.OpenTime)
	if err != nil {
		return "", err
	}

	daysUntil := (meta.OpenDayOfWeek - int(nowInLoc.Weekday()) + 7) % 7
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
