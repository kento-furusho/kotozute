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
// 区切りは遠い過去のsentinelにし、「これまで書いた全部を書くサイクル扱い」にする。
const (
	defaultCycleStartedAt = "1970-01-01T00:00:00Z"
	defaultOpenDayOfWeek  = 5 // time.Weekday: 0=Sun...6=Sat → 5=金曜(仮)
	defaultOpenTime       = "20:00"
	defaultTimezone       = "Asia/Tokyo"
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
	CreatedAt string
}

type kotozuteResponseItem struct {
	ID        string `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
}

type thisWeekResponse struct {
	MyKotozute        []kotozuteResponseItem `json:"myKotozute"`
	OpenStatus        string                 `json:"openStatus"`
	CanSeePartner     bool                   `json:"canSeePartner"`
	PartnerKotozute   []kotozuteResponseItem `json:"partnerKotozute,omitempty"`
	PartnerHasWritten bool                   `json:"partnerHasWritten"`
	NextOpenAt        string                 `json:"nextOpenAt"`
}

type pairMeta struct {
	// 書くサイクルの開始（＝前回オープンした時刻）。
	CycleStartedAt string
	// 読むサイクルの開始（＝前々回オープンした時刻）。
	PreviousCycleStartedAt string
	OpenDayOfWeek          int
	OpenTime               string
	Timezone               string
	Member1                string // ペアの片方のユーザーID
	Member2                string // ペアのもう片方のユーザーID
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

	// 4. 読むサイクルの頭から現在までをまとめて引き、2つのサイクルに振り分ける。
	//    2回のQueryで、自分/相手 × 読む/書く の4組が揃う。
	myPrevious, myCurrent, err := queryCycles(ctx, pairID, userID, meta)
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to fetch kotozute"), nil
	}
	partnerPrevious, partnerCurrent, err := queryCycles(ctx, pairID, partnerID, meta)
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to fetch kotozute"), nil
	}

	nextOpenAt, err := computeNextOpenAt(meta, time.Now())
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to compute next open date"), nil
	}

	// 5. 相互ロック判定（純粋関数）でレスポンスを組み立てる。
	return okJSON(buildThisWeekResponse(myPrevious, myCurrent, partnerPrevious, partnerCurrent, nextOpenAt))
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
//
// 引数は4組:
//   - myPrevious      … 読むサイクルに自分が書いた分（今週ちゃんと書いた人かの判定に使う）
//   - myCurrent       … 書くサイクルに自分が書いた分（画面に出す「今週のことづて」）
//   - partnerPrevious … 読むサイクルの相手の分（解禁対象。本文を返すのはここだけ）
//   - partnerCurrent  … 書くサイクルの相手の分（気配表示の有無だけに使う）
//
// canSeePartner=false のときは PartnerKotozute に一切触れない（本文を詰めるコードパス自体に
// 到達しない）ことで、本文漏洩を構造的に避ける。
func buildThisWeekResponse(myPrevious, myCurrent, partnerPrevious, partnerCurrent []kotozuteItem, nextOpenAt string) thisWeekResponse {
	// 読むサイクルに相手の分があれば「オープン済み」。バッチが区切りを進めた結果なので、
	// ことづて1枚ごとのフラグを見る必要はない。
	partnerOpened := len(partnerPrevious) > 0

	// 自分は2つのサイクルのどちらかに書いていればよい。
	// 読むサイクル … その週ちゃんと書いた人。オープン直後にそのまま読める。
	// 書くサイクル … その週は書かなかったが、いま1枚書いた人。これで解禁される。
	iHaveWritten := len(myPrevious) > 0 || len(myCurrent) > 0
	canSeePartner := partnerOpened && iHaveWritten

	openStatus := "before"
	if partnerOpened {
		openStatus = "opened"
	}

	res := thisWeekResponse{
		MyKotozute:    toResponseItems(myCurrent),
		OpenStatus:    openStatus,
		CanSeePartner: canSeePartner,
		// 気配は「いま書いているサイクル」の話。枚数は返さない。
		PartnerHasWritten: len(partnerCurrent) > 0,
		NextOpenAt:        nextOpenAt,
	}
	if canSeePartner {
		res.PartnerKotozute = toResponseItems(partnerPrevious)
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

// queryCycles は読むサイクルの頭から現在までを1回のQueryで引き、
// cycleStartedAt を境に「読むサイクル」と「書くサイクル」へ振り分ける。
//
// SK は KOTOZUTE#<書いた人>#<ISO日時>#<id> で時刻が昇順に並ぶので、
// 範囲検索がそのまま期間の切り出しになる。opened フラグでの絞り込みは不要。
//
// DynamoDBは1つのソートキーに1条件式しか使えないため、begins_with と範囲条件を
// 同時に満たすべくBETWEENで組み立てる（上限のUnicode最大文字￿で、この書いた人の
// プレフィックス内の最大値を保証する定石パターン）。
func queryCycles(ctx context.Context, pairID, userID string, meta pairMeta) (previous, current []kotozuteItem, err error) {
	lower := fmt.Sprintf("KOTOZUTE#%s#%s", userID, meta.PreviousCycleStartedAt)
	upper := fmt.Sprintf("KOTOZUTE#%s#￿", userID)
	boundary := fmt.Sprintf("KOTOZUTE#%s#%s", userID, meta.CycleStartedAt)

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
		return nil, nil, err
	}

	previous = make([]kotozuteItem, 0, len(out.Items))
	current = make([]kotozuteItem, 0, len(out.Items))
	for _, av := range out.Items {
		sk, ok := av["SK"].(*types.AttributeValueMemberS)
		if !ok {
			continue
		}
		item, err := parseKotozuteItem(av)
		if err != nil {
			return nil, nil, err
		}
		if sk.Value < boundary {
			previous = append(previous, item)
		} else {
			current = append(current, item)
		}
	}
	return previous, current, nil
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
	createdAt, ok := av["createdAt"].(*types.AttributeValueMemberS)
	if !ok {
		return kotozuteItem{}, fmt.Errorf("createdAt missing or invalid type")
	}

	return kotozuteItem{
		ID:        parts[3],
		Body:      body.Value,
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
		CycleStartedAt:         defaultCycleStartedAt,
		PreviousCycleStartedAt: defaultCycleStartedAt,
		OpenDayOfWeek:          defaultOpenDayOfWeek,
		OpenTime:               defaultOpenTime,
		Timezone:               defaultTimezone,
	}
	if len(item) == 0 {
		return meta
	}

	if v, ok := item["cycleStartedAt"].(*types.AttributeValueMemberS); ok {
		meta.CycleStartedAt = v.Value
	}
	if v, ok := item["previousCycleStartedAt"].(*types.AttributeValueMemberS); ok {
		meta.PreviousCycleStartedAt = v.Value
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
