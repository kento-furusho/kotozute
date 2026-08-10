package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
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
	PairID        string
	Member1       string
	Member2       string
	OpenDayOfWeek int
	OpenTime      string
	Timezone      string
	LastOpenedAt  string
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
// - かつ、このサイクルではまだオープンしていない（lastOpenedAt が今回のオープン時刻より前）
// 二重オープン防止は lastOpenedAt との比較で行うため、バッチが毎時走っても1サイクル1回に収まる。
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
	if last, err := time.Parse(time.RFC3339, pair.LastOpenedAt); err == nil && !last.Before(openMoment) {
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
	if v, ok := item["lastOpenedAt"].(*types.AttributeValueMemberS); ok {
		p.LastOpenedAt = v.Value
	}
	return p
}

// openPair はこのサイクル分の未オープンことづてを opened=true にし、lastOpenedAt を更新する。
// 両者ゼロの週でも lastOpenedAt は更新してサイクルを前進させる（静かにスキップ）。
func openPair(ctx context.Context, pair pairMeta, now time.Time) error {
	nowStr := now.UTC().Format(time.RFC3339)

	// このサイクル分の未オープンことづて。SK に「書いた人」が入っているので、
	// オープン前にここから「誰が書いたか」を割り出せる。
	sks, err := queryCycleKotozuteSKs(ctx, pair)
	if err != nil {
		return err
	}
	wroteMember1 := someoneWrote(sks, pair.Member1)
	wroteMember2 := someoneWrote(sks, pair.Member2)

	for _, sk := range sks {
		if err := markOpened(ctx, pair.PairID, sk, nowStr); err != nil {
			return err
		}
	}
	if err := updateLastOpenedAt(ctx, pair.PairID, nowStr); err != nil {
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

// someoneWrote は SK 群の中に、その userID が書いたことづてがあるかを返す。
// SK は "KOTOZUTE#<書いた人>#..." なので、プレフィックスで判定できる純粋関数。
func someoneWrote(sks []string, userID string) bool {
	prefix := fmt.Sprintf("KOTOZUTE#%s#", userID)
	for _, sk := range sks {
		if strings.HasPrefix(sk, prefix) {
			return true
		}
	}
	return false
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
	_, err := snsClient.Publish(ctx, &sns.PublishInput{
		TargetArn: aws.String(arn),
		Message:   aws.String(n.Message),
	})
	return err
}

// queryCycleKotozuteSKs はこのサイクル（lastOpenedAt 以降）の未オープンことづての SK を集める。
// createdAt >= lastOpenedAt かつ opened=false を FilterExpression で絞る。
func queryCycleKotozuteSKs(ctx context.Context, pair pairMeta) ([]string, error) {
	out, err := dynamoClient.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(tableName),
		KeyConditionExpression: aws.String("PK = :pk AND begins_with(SK, :prefix)"),
		FilterExpression:       aws.String("createdAt >= :since AND #opened = :false"),
		ExpressionAttributeNames: map[string]string{
			"#opened": "opened",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":     &types.AttributeValueMemberS{Value: fmt.Sprintf("PAIR#%s", pair.PairID)},
			":prefix": &types.AttributeValueMemberS{Value: "KOTOZUTE#"},
			":since":  &types.AttributeValueMemberS{Value: pair.LastOpenedAt},
			":false":  &types.AttributeValueMemberBOOL{Value: false},
		},
	})
	if err != nil {
		return nil, err
	}

	sks := make([]string, 0, len(out.Items))
	for _, item := range out.Items {
		if sk, ok := item["SK"].(*types.AttributeValueMemberS); ok {
			sks = append(sks, sk.Value)
		}
	}
	return sks, nil
}

// markOpened は1件のことづてを opened=true にし、openedAt を記録する。
// opened=false を条件にして、二重更新（既にオープン済み）を避ける。
func markOpened(ctx context.Context, pairID, sk, nowStr string) error {
	_, err := dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("PAIR#%s", pairID)},
			"SK": &types.AttributeValueMemberS{Value: sk},
		},
		UpdateExpression:    aws.String("SET #opened = :true, openedAt = :now"),
		ConditionExpression: aws.String("#opened = :false"),
		ExpressionAttributeNames: map[string]string{
			"#opened": "opened",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":true":  &types.AttributeValueMemberBOOL{Value: true},
			":false": &types.AttributeValueMemberBOOL{Value: false},
			":now":   &types.AttributeValueMemberS{Value: nowStr},
		},
	})
	if err != nil {
		// 直前に他の実行がオープン済みにした場合は条件エラーになるが、結果は同じなので無視してよい。
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return nil
		}
		return err
	}
	return nil
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

func updateLastOpenedAt(ctx context.Context, pairID, nowStr string) error {
	_, err := dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("PAIR#%s", pairID)},
			"SK": &types.AttributeValueMemberS{Value: "META"},
		},
		UpdateExpression: aws.String("SET lastOpenedAt = :now"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":now": &types.AttributeValueMemberS{Value: nowStr},
		},
	})
	return err
}

func main() {
	lambda.Start(handler)
}
