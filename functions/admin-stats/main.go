// admin-stats は管理用の集計を返す。
//
// API Gateway にはつながない（template.yaml で Events を持たせない）。
// 呼べるのは、この AWS アカウントで lambda:InvokeFunction の権限を持つ IAM だけ。
// 手元の scripts/admin/dashboard.py が `aws lambda invoke` で呼ぶ。
//
// 数えるだけで、ことづての本文には触れない。Scan の ProjectionExpression で
// キーと作成日時だけを取り出すので、本文はこの Lambda にも渡ってこない。
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const tableName = "kotozute"

// 週ごとの推移を何週ぶん返すか。
const weeksToReport = 12

// 週の区切りは日本時間の月曜 0:00。利用者がほぼ日本にいる前提。
// tzdata に頼らないよう固定オフセットにする。
var jst = time.FixedZone("JST", 9*60*60)

var dynamoClient *dynamodb.Client

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

// row は集計に使う1行。キーと作成日時だけ。
type row struct {
	PK        string
	SK        string
	CreatedAt string
}

type weekStats struct {
	WeekStart string `json:"weekStart"` // 日本時間の月曜（"2026-11-02"）
	Kotozute  int    `json:"kotozute"`
	NewUsers  int    `json:"newUsers"`
	NewPairs  int    `json:"newPairs"`
	Feedback  int    `json:"feedback"`
}

type stats struct {
	GeneratedAt string `json:"generatedAt"`
	Users       int    `json:"users"`
	Pairs       int    `json:"pairs"`
	Kotozute    int    `json:"kotozute"`
	Feedback    int    `json:"feedback"`
	// 直近に1枚以上書いた人の数。「使い続けているか」の目安。
	ActiveWriters7d  int `json:"activeWriters7d"`
	ActiveWriters30d int `json:"activeWriters30d"`
	// 作成日時を持たない古いユーザー・ペアの数。週ごとの推移には入らない。
	UsersWithoutCreatedAt int         `json:"usersWithoutCreatedAt"`
	PairsWithoutCreatedAt int         `json:"pairsWithoutCreatedAt"`
	Weekly                []weekStats `json:"weekly"`
}

func handler(ctx context.Context) (stats, error) {
	rows, err := scanKeys(ctx)
	if err != nil {
		return stats{}, err
	}
	return aggregate(rows, time.Now()), nil
}

// scanKeys はテーブル全体から、キーと作成日時だけを読む。
// 件数が少ないうちは Scan で十分。増えたら集計用の項目を別に持つ形に変える。
func scanKeys(ctx context.Context) ([]row, error) {
	var rows []row
	var startKey map[string]types.AttributeValue
	for {
		out, err := dynamoClient.Scan(ctx, &dynamodb.ScanInput{
			TableName:            aws.String(tableName),
			ProjectionExpression: aws.String("PK, SK, createdAt"),
			ExclusiveStartKey:    startKey,
		})
		if err != nil {
			return nil, err
		}
		for _, item := range out.Items {
			rows = append(rows, row{
				PK:        stringAttr(item, "PK"),
				SK:        stringAttr(item, "SK"),
				CreatedAt: stringAttr(item, "createdAt"),
			})
		}
		if len(out.LastEvaluatedKey) == 0 {
			return rows, nil
		}
		startKey = out.LastEvaluatedKey
	}
}

// timeFromSK は "KOTOZUTE#<ISO日時>#<id>" のような SK から日時を取り出す。
func timeFromSK(sk string) (time.Time, bool) {
	parts := strings.SplitN(sk, "#", 3)
	if len(parts) < 2 {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, parts[1])
	return t, err == nil
}

// weekStart は、その時刻が入る週の月曜 0:00（日本時間）を返す。
func weekStart(t time.Time) time.Time {
	local := t.In(jst)
	offset := (int(local.Weekday()) + 6) % 7 // 月曜=0
	day := local.AddDate(0, 0, -offset)
	return time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, jst)
}

func aggregate(rows []row, now time.Time) stats {
	s := stats{GeneratedAt: now.UTC().Format(time.RFC3339)}

	// 直近 weeksToReport 週の箱を先に作っておく（0件の週も並べるため）。
	current := weekStart(now)
	weeks := make(map[string]*weekStats, weeksToReport)
	order := make([]string, 0, weeksToReport)
	for i := weeksToReport - 1; i >= 0; i-- {
		key := current.AddDate(0, 0, -7*i).Format("2006-01-02")
		weeks[key] = &weekStats{WeekStart: key}
		order = append(order, key)
	}
	bucket := func(t time.Time) *weekStats {
		return weeks[weekStart(t).Format("2006-01-02")]
	}

	writers7d := map[string]bool{}
	writers30d := map[string]bool{}

	for _, r := range rows {
		switch {
		case strings.HasPrefix(r.PK, "USER#") && r.SK == "META":
			s.Users++
			if t, err := time.Parse(time.RFC3339, r.CreatedAt); err == nil {
				if w := bucket(t); w != nil {
					w.NewUsers++
				}
			} else {
				s.UsersWithoutCreatedAt++
			}
		case strings.HasPrefix(r.PK, "PAIR#") && r.SK == "META":
			s.Pairs++
			if t, err := time.Parse(time.RFC3339, r.CreatedAt); err == nil {
				if w := bucket(t); w != nil {
					w.NewPairs++
				}
			} else {
				s.PairsWithoutCreatedAt++
			}
		case strings.HasPrefix(r.PK, "USER#") && strings.HasPrefix(r.SK, "KOTOZUTE#"):
			s.Kotozute++
			t, ok := timeFromSK(r.SK)
			if !ok {
				continue
			}
			if w := bucket(t); w != nil {
				w.Kotozute++
			}
			age := now.Sub(t)
			if age <= 7*24*time.Hour {
				writers7d[r.PK] = true
			}
			if age <= 30*24*time.Hour {
				writers30d[r.PK] = true
			}
		case strings.HasPrefix(r.PK, "USER#") && strings.HasPrefix(r.SK, "FEEDBACK#"):
			s.Feedback++
			if t, ok := timeFromSK(r.SK); ok {
				if w := bucket(t); w != nil {
					w.Feedback++
				}
			}
		}
	}

	s.ActiveWriters7d = len(writers7d)
	s.ActiveWriters30d = len(writers30d)

	sort.Strings(order)
	for _, key := range order {
		s.Weekly = append(s.Weekly, *weeks[key])
	}
	return s
}

func stringAttr(item map[string]types.AttributeValue, name string) string {
	if v, ok := item[name].(*types.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}

func main() {
	lambda.Start(handler)
}
