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

// 認証導入までは「自分」をk01に固定する。Cognito導入後、トークンから取得する形に差し替える。
const fixedUserID = "k01"

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
}

func handler(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	userOut, err := dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("USER#%s", fixedUserID)},
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
		UserID:     fixedUserID,
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
	return meta
}

// computeNextOpenAt はmetaのopenDayOfWeek/openTime/timezoneから直近の次回オープン日時を算出する。
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
