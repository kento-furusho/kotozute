package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // Lambda 実行環境に IANA タイムゾーンDBが無くても LoadLocation を効かせる

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
)

const tableName = "kotozute"

// バッチが「今日は何曜日か」を判定する基準タイムゾーン（当面は日本固定）。
// マルチタイムゾーン対応は将来課題。実際のオープン可否は各ペアの timezone で再判定する。
const referenceTimezone = "Asia/Tokyo"

var (
	dynamoClient *dynamodb.Client
	snsClient    *sns.Client
)

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
	snsClient = sns.NewFromConfig(cfg)
}

// notification は「誰に」「どんな文面を」送るか。
type notification struct {
	UserID  string
	Message string
}

type pairMeta struct {
	PairID         string
	Member1        string
	Member2        string
	OpenDayOfWeek  int
	OpenTime       string
	Timezone       string
	CycleStartedAt string
	// 直前の区切り。履歴を持たない古いペアの取りこぼしを埋めるために読む。
	PreviousCycleStartedAt string
	// 区切りの履歴（古い順）。週ごとに遡るために使う。
	CycleStarts []string
}

// handler は EventBridge から定期起動される。API Gateway からは呼ばれないので
// リクエスト/レスポンス型は取らず、ctx と error だけのシンプルな形になる。
func handler(ctx context.Context) error {
	now := time.Now()

	refLoc, err := time.LoadLocation(referenceTimezone)
	if err != nil {
		refLoc = time.UTC
	}
	weekday := int(now.In(refLoc).Weekday())

	pairs, err := queryPairsByDay(ctx, weekday)
	if err != nil {
		return fmt.Errorf("failed to query pairs: %w", err)
	}

	for _, p := range pairs {
		if !shouldOpen(p, now) {
			continue
		}
		if err := openPair(ctx, p, now); err != nil {
			// 1ペアの失敗で全体を止めない。ログに残して次へ進む（再実行は冪等）。
			log.Printf("failed to open pair %s: %v", p.PairID, err)
			continue
		}
		log.Printf("opened pair %s", p.PairID)
	}
	return nil
}

// shouldOpen はペアの timezone で「いまオープンすべきか」を判定する純粋関数。
// - 今がそのペアのオープン曜日で、オープン時刻を過ぎている
// - かつ、このサイクルではまだオープンしていない（cycleStartedAt が今回のオープン時刻より前）
// 二重オープン防止は cycleStartedAt との比較で行うため、バッチが毎時走っても1サイクル1回に収まる。
func shouldOpen(pair pairMeta, now time.Time) bool {
	loc, err := time.LoadLocation(pair.Timezone)
	if err != nil {
		loc = time.UTC
	}
	nowLocal := now.In(loc)

	if int(nowLocal.Weekday()) != pair.OpenDayOfWeek {
		return false
	}

	hour, minute, err := parseHHMM(pair.OpenTime)
	if err != nil {
		return false
	}
	openMoment := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), hour, minute, 0, 0, loc)
	if nowLocal.Before(openMoment) {
		return false // まだ今日のオープン時刻に達していない
	}

	// 今回のオープン時刻以降に既にオープン済みなら、このサイクルは処理済み。
	if started, err := time.Parse(time.RFC3339, pair.CycleStartedAt); err == nil && !started.Before(openMoment) {
		return false
	}
	return true
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

// queryPairsByDay は GSI1 で openDayOfWeek が weekday のペアを検索する。
func queryPairsByDay(ctx context.Context, weekday int) ([]pairMeta, error) {
	out, err := dynamoClient.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(tableName),
		IndexName:              aws.String("GSI1"),
		KeyConditionExpression: aws.String("openDayOfWeek = :d"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":d": &types.AttributeValueMemberN{Value: strconv.Itoa(weekday)},
		},
	})
	if err != nil {
		return nil, err
	}

	pairs := make([]pairMeta, 0, len(out.Items))
	for _, item := range out.Items {
		// PAIR META（SK=META）だけを対象にする。
		if sk, ok := item["SK"].(*types.AttributeValueMemberS); !ok || sk.Value != "META" {
			continue
		}
		pairs = append(pairs, buildPairMeta(item))
	}
	return pairs, nil
}

func buildPairMeta(item map[string]types.AttributeValue) pairMeta {
	p := pairMeta{}
	if v, ok := item["PK"].(*types.AttributeValueMemberS); ok {
		p.PairID = strings.TrimPrefix(v.Value, "PAIR#")
	}
	if v, ok := item["member1"].(*types.AttributeValueMemberS); ok {
		p.Member1 = v.Value
	}
	if v, ok := item["member2"].(*types.AttributeValueMemberS); ok {
		p.Member2 = v.Value
	}
	if v, ok := item["openDayOfWeek"].(*types.AttributeValueMemberN); ok {
		if n, err := strconv.Atoi(v.Value); err == nil {
			p.OpenDayOfWeek = n
		}
	}
	if v, ok := item["openTime"].(*types.AttributeValueMemberS); ok {
		p.OpenTime = v.Value
	}
	if v, ok := item["timezone"].(*types.AttributeValueMemberS); ok {
		p.Timezone = v.Value
	}
	if v, ok := item["cycleStartedAt"].(*types.AttributeValueMemberS); ok {
		p.CycleStartedAt = v.Value
	}
	if v, ok := item["previousCycleStartedAt"].(*types.AttributeValueMemberS); ok {
		p.PreviousCycleStartedAt = v.Value
	}
	if v, ok := item["cycleStarts"].(*types.AttributeValueMemberL); ok {
		for _, e := range v.Value {
			if s, ok := e.(*types.AttributeValueMemberS); ok {
				p.CycleStarts = append(p.CycleStarts, s.Value)
			}
		}
	}
	return p
}

// openPair は区切りを1つ前に進める。ことづては1枚も書き換えない。
//
//	previousCycleStartedAt ← cycleStartedAt   （いま閉じたサイクルが「読むサイクル」になる）
//	cycleStartedAt         ← 現在             （ここから次のサイクルを書き始める）
//
// 両者ゼロの週でも区切りは進めてサイクルを前進させる（静かにスキップ）。
func openPair(ctx context.Context, pair pairMeta, now time.Time) error {
	nowStr := now.UTC().Format(time.RFC3339)

	// 通知の出し分けに「このサイクルで誰が書いたか」が要るので読むだけ読む。更新はしない。
	// ことづては書いた人のパーティションにあるので、メンバーごとに1件あるかを見る。
	wroteMember1, err := wroteInCycle(ctx, pair.Member1, pair.CycleStartedAt)
	if err != nil {
		return err
	}
	wroteMember2, err := wroteInCycle(ctx, pair.Member2, pair.CycleStartedAt)
	if err != nil {
		return err
	}

	if err := advanceCycle(ctx, pair, nowStr); err != nil {
		return err
	}

	// 通知の出し分け。両者ゼロの週は buildNotifications が空を返すので誰にも送らない。
	name1 := getUserDisplayName(ctx, pair.Member1)
	name2 := getUserDisplayName(ctx, pair.Member2)
	for _, n := range buildNotifications(pair, wroteMember1, wroteMember2, name1, name2) {
		if err := sendNotification(ctx, n); err != nil {
			// 通知失敗はオープン自体の成否と切り離す（ログのみ）。
			log.Printf("failed to notify user %s: %v", n.UserID, err)
		}
	}
	return nil
}

// buildNotifications は通知の出し分けを決める純粋関数。
// - 両者ゼロ（誰も書いていない）→ 通知しない（空）
// - 書いた人 → 「今週のことづて、オープンしました」
// - 書いてない人 → 「あなたが1つ書くと、○○のことづても見られます」（催促でなく事実）
func buildNotifications(pair pairMeta, wroteMember1, wroteMember2 bool, name1, name2 string) []notification {
	if !wroteMember1 && !wroteMember2 {
		return nil
	}
	return []notification{
		notificationFor(pair.Member1, wroteMember1, name2), // member1 の相手は member2
		notificationFor(pair.Member2, wroteMember2, name1),
	}
}

func notificationFor(userID string, wrote bool, partnerName string) notification {
	if wrote {
		return notification{UserID: userID, Message: "今週のことづて、オープンしました"}
	}
	if partnerName == "" {
		partnerName = "相手"
	}
	return notification{UserID: userID, Message: fmt.Sprintf("あなたが1つ書くと、%sのことづても見られます", partnerName)}
}

// sendNotification は SNS 経由で1件通知する。
// ユーザーの SNS エンドポイントARN（USER META の snsEndpointArn）が未登録なら、
// 送信先が無いのでログだけ残してスキップする（APNsエンドポイント登録は別タスク）。
func sendNotification(ctx context.Context, n notification) error {
	arn := getUserSNSEndpoint(ctx, n.UserID)
	if arn == "" {
		log.Printf("no SNS endpoint for user %s; skip. message=%q", n.UserID, n.Message)
		return nil
	}
	payload, err := buildPushPayload(arn, n.Message)
	if err != nil {
		return err
	}
	_, err = snsClient.Publish(ctx, &sns.PublishInput{
		TargetArn: aws.String(arn),
		Message:   aws.String(payload),
		// これを付けないと Message が素の文字列のまま APNs へ渡り、表示されない。
		MessageStructure: aws.String("json"),
	})
	return err
}

// buildPushPayload は SNS の「プラットフォームごとに中身を変える」形の Message を作る。
//
// APNs は aps を含む JSON でないと通知を表示しない。SNS に素の文字列を渡すと
// そのまま APNs へ送られ、端末には何も出ない（送信自体は成功して見える）。
//
// 外側は {"default": ..., "APNS_SANDBOX": "<JSON文字列>"} の形。
// APNs 向けの値が「JSON を文字列にしたもの」なのは SNS の仕様。
// default は必須で、構造化を解さない経路への保険になる。
func buildPushPayload(endpointArn, message string) (string, error) {
	aps, err := json.Marshal(map[string]any{
		"aps": map[string]any{
			"alert": message,
			"sound": "default",
		},
	})
	if err != nil {
		return "", err
	}
	outer, err := json.Marshal(map[string]string{
		"default":                            message,
		platformFromEndpointARN(endpointArn): string(aps),
	})
	if err != nil {
		return "", err
	}
	return string(outer), nil
}

// platformFromEndpointARN はエンドポイントARNからプラットフォーム名を取り出す。
//
//	arn:aws:sns:<region>:<account>:endpoint/APNS_SANDBOX/<app>/<uuid>
//	                                        ^^^^^^^^^^^^
//
// 開発用（APNS_SANDBOX）か本番用（APNS）かでキー名が変わるので、
// 設定を増やさずに ARN 自身から決める。読めなければ本番として扱う。
func platformFromEndpointARN(arn string) string {
	_, resource, found := strings.Cut(arn, ":endpoint/")
	if !found {
		return "APNS"
	}
	platform, _, found := strings.Cut(resource, "/")
	if !found || platform == "" {
		return "APNS"
	}
	return platform
}

// wroteInCycle は、そのユーザーがいま閉じるサイクル（cycleStartedAt 以降）に
// 1枚でも書いたかを返す。通知の出し分けにしか使わないので、1件見つかれば十分。
func wroteInCycle(ctx context.Context, userID, cycleStartedAt string) (bool, error) {
	out, err := dynamoClient.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(tableName),
		KeyConditionExpression: aws.String("PK = :pk AND SK BETWEEN :lower AND :upper"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":    &types.AttributeValueMemberS{Value: fmt.Sprintf("USER#%s", userID)},
			":lower": &types.AttributeValueMemberS{Value: fmt.Sprintf("KOTOZUTE#%s", cycleStartedAt)},
			":upper": &types.AttributeValueMemberS{Value: "KOTOZUTE#￿"},
		},
		Limit: aws.Int32(1),
	})
	if err != nil {
		return false, err
	}
	return len(out.Items) > 0, nil
}

func getUserDisplayName(ctx context.Context, userID string) string {
	return getUserAttr(ctx, userID, "displayName")
}

func getUserSNSEndpoint(ctx context.Context, userID string) string {
	return getUserAttr(ctx, userID, "snsEndpointArn")
}

// getUserAttr は USER#<userID>/META の指定した文字列属性を読む。無ければ空文字。
func getUserAttr(ctx context.Context, userID, attr string) string {
	out, err := dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("USER#%s", userID)},
			"SK": &types.AttributeValueMemberS{Value: "META"},
		},
	})
	if err != nil {
		return ""
	}
	if v, ok := out.Item[attr].(*types.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}

// nextCycleStarts はオープン後の区切りの履歴を組み立てる。
//
// 履歴に加えて、ペアが持っている2つの区切りも必ず混ぜる。
// 履歴を後から足した都合で、それ以前の区切りが履歴に入っていないペアがあるため。
// ISO8601 の文字列は辞書順が時刻順と一致するので、文字列のまま並べてよい。
func nextCycleStarts(pair pairMeta, nowStr string) []string {
	all := append([]string{}, pair.CycleStarts...)
	all = append(all, pair.PreviousCycleStartedAt, pair.CycleStartedAt, nowStr)

	seen := map[string]bool{}
	out := make([]string, 0, len(all))
	for _, v := range all {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func cycleStartsValue(starts []string) types.AttributeValue {
	values := make([]types.AttributeValue, 0, len(starts))
	for _, s := range starts {
		values = append(values, &types.AttributeValueMemberS{Value: s})
	}
	return &types.AttributeValueMemberL{Value: values}
}

// advanceCycle は区切りを1つ前へ進める。オープン処理の実体はこれだけ。
//
// 条件式で「読み取った時点の cycleStartedAt から変わっていないこと」を確かめる。
// バッチが重なって走っても、区切りが二重に進まない。
func advanceCycle(ctx context.Context, pair pairMeta, nowStr string) error {
	_, err := dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("PAIR#%s", pair.PairID)},
			"SK": &types.AttributeValueMemberS{Value: "META"},
		},
		// 区切りを進めると同時に、履歴も書き直す。
		// list_append で継ぎ足すのではなく毎回まるごと書くのは、
		// 履歴を持たずに始まったペアの区切りを取りこぼさないため。
		UpdateExpression:    aws.String("SET previousCycleStartedAt = :prev, cycleStartedAt = :now, cycleStarts = :starts"),
		ConditionExpression: aws.String("cycleStartedAt = :prev"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":prev":   &types.AttributeValueMemberS{Value: pair.CycleStartedAt},
			":now":    &types.AttributeValueMemberS{Value: nowStr},
			":starts": cycleStartsValue(nextCycleStarts(pair, nowStr)),
		},
	})
	if err != nil {
		// 他の実行が先に進めた場合。結果は同じなので成功扱いにする。
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return nil
		}
		return err
	}
	return nil
}

func main() {
	lambda.Start(handler)
}
