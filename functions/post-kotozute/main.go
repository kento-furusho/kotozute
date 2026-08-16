package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/google/uuid"
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

type postKotozuteRequest struct {
	Body string `json:"body"`
}

type kotozuteResponse struct {
	ID   string `json:"id"`
	Body string `json:"body"`
	// 書いた直後は必ず未オープン。保存はせず、この場で false を返すだけにする。
	Opened    bool   `json:"opened"`
	CreatedAt string `json:"createdAt"`
}

func handler(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	// ことづてを書くのはログイン済みの本人。トークンからそのユーザーIDを得る。
	userID, ok := userIDFromRequest(request)
	if !ok {
		return errorResponse(http.StatusUnauthorized, "unauthorized"), nil
	}

	// 先に安い入力チェックを済ませる（DBアクセスは最後まで遅らせる）。
	var req postKotozuteRequest
	if err := json.Unmarshal([]byte(request.Body), &req); err != nil {
		return errorResponse(http.StatusBadRequest, "invalid request body"), nil
	}
	if req.Body == "" {
		return errorResponse(http.StatusBadRequest, "body is required"), nil
	}

	// ことづては書いた人の下に置く。ペアは要らない。
	// オンボーディングで「最初の1枚」を書いてから招待する導線を成立させるため。
	createdAt := time.Now().UTC().Format(time.RFC3339)
	kotozuteID := uuid.NewString()

	item := map[string]types.AttributeValue{
		// 書いた人がパーティション。SK に書いた人を入れる必要はなくなった。
		"PK":   &types.AttributeValueMemberS{Value: fmt.Sprintf("USER#%s", userID)},
		"SK":   &types.AttributeValueMemberS{Value: fmt.Sprintf("KOTOZUTE#%s#%s", createdAt, kotozuteID)},
		"body": &types.AttributeValueMemberS{Value: req.Body},
		// opened フラグは持たせない。オープン済みかどうかはペアの cycleStartedAt との
		// 比較で決まるので、1枚ごとに状態を分散させない。
		"createdAt": &types.AttributeValueMemberS{Value: createdAt},
	}

	if _, err := dynamoClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(tableName),
		Item:      item,
	}); err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to save kotozute"), nil
	}

	resBody, err := json.Marshal(kotozuteResponse{
		ID:        kotozuteID,
		Body:      req.Body,
		Opened:    false,
		CreatedAt: createdAt,
	})
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "failed to build response"), nil
	}

	return events.APIGatewayProxyResponse{
		StatusCode: http.StatusCreated,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(resBody),
	}, nil
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
