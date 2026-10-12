package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"
)

const tableName = "kotozute"

var dynamoClient *dynamodb.Client
var snsClient *sns.Client

// SNS のプラットフォームアプリケーション（APNs）の ARN。開発用と配布用で別物。
//
// 1つのバックエンドが両方の端末を相手にするが、トークンを見ても
// どちらの環境のものかは分からない。端末が自分の環境を送ってくるので、それで選ぶ。
// 取り違えると APNs に拒否され、エンドポイントが無効化されて二度と届かなくなる。
//
// 空のときはその環境の端末を登録せず、apnsToken の保存だけで済ませる。
var platformApplicationArn string
var platformApplicationArnSandbox string

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
	snsClient = sns.NewFromConfig(cfg)
	platformApplicationArn = os.Getenv("SNS_PLATFORM_APPLICATION_ARN")
	platformApplicationArnSandbox = os.Getenv("SNS_PLATFORM_APPLICATION_ARN_SANDBOX")
}

// ポインタ型にすることで「JSONにキーが無い（nil）」と「空文字を送ってきた」を区別する。
// nil のフィールドは更新対象にしない。
type patchMeRequest struct {
	DisplayName *string `json:"displayName"`
	ApnsToken   *string `json:"apnsToken"`
	// "sandbox" なら開発ビルド。省略・それ以外は配布ビルドとして扱う。
	// 配れば大半が配布ビルドなので、迷ったらそちらに倒す。
	ApnsEnvironment *string `json:"apnsEnvironment"`
}

// platformApplicationFor は端末の環境に合う SNS のプラットフォームアプリケーションを選ぶ。
func platformApplicationFor(environment *string) string {
	if environment != nil && *environment == "sandbox" {
		return platformApplicationArnSandbox
	}
	return platformApplicationArn
}

// 表示名の上限。相手の画面では「◯◯からのことづて」の◯◯に入るので、
// 長すぎると見出しが折り返して読みにくくなる。
const maxDisplayNameRunes = 20

// validateDisplayName は表示名を検証し、前後の空白を落としたものを返す。
// 不正なら errMsg（400用）を返す。
//
// 空を通すと、相手の画面の「◯◯からのことづて」やロック画面の文言が
// 崩れる。長さは文字数で数える（日本語はバイト数だとすぐ上限に当たる）。
func validateDisplayName(name string) (string, string) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", "displayName must not be empty"
	}
	if utf8.RuneCountInString(trimmed) > maxDisplayNameRunes {
		return "", "displayName is too long"
	}
	return trimmed, ""
}

// buildUpdateExpression は、nil でないフィールドだけを更新する UpdateExpression と
// その値マップを組み立てる純粋関数。更新対象が1つも無ければ ok=false を返す。
//
// snsEndpointArn は端末の登録に成功したときだけ渡ってくる。空なら触らない
// （登録に失敗しても apnsToken の保存は済ませたいので、更新対象から外すだけにする）。
//
// **apnsToken が空文字なら「この端末の登録を外す」。** サインアウトのときに送られてくる。
// 空文字を SET すると宛先（snsEndpointArn）だけが残り、サインアウトしたあとも
// 前のアカウント宛ての通知がその端末に届き続ける（実際にそうなっていた）。
func buildUpdateExpression(req patchMeRequest, snsEndpointArn string) (string, map[string]types.AttributeValue, bool) {
	setClauses := make([]string, 0, 3)
	removeClauses := make([]string, 0, 3)
	values := map[string]types.AttributeValue{}
	if req.DisplayName != nil {
		setClauses = append(setClauses, "displayName = :displayName")
		values[":displayName"] = &types.AttributeValueMemberS{Value: *req.DisplayName}
	}
	if req.ApnsToken != nil {
		if *req.ApnsToken == "" {
			removeClauses = append(removeClauses, "apnsToken", "snsEndpointArn")
		} else {
			setClauses = append(setClauses, "apnsToken = :apnsToken")
			values[":apnsToken"] = &types.AttributeValueMemberS{Value: *req.ApnsToken}
		}
	}
	if snsEndpointArn != "" {
		setClauses = append(setClauses, "snsEndpointArn = :snsEndpointArn")
		values[":snsEndpointArn"] = &types.AttributeValueMemberS{Value: snsEndpointArn}
	}
	if len(setClauses) == 0 && len(removeClauses) == 0 {
		return "", nil, false
	}
	parts := make([]string, 0, 2)
	if len(setClauses) > 0 {
		parts = append(parts, "SET "+strings.Join(setClauses, ", "))
	}
	if len(removeClauses) > 0 {
		parts = append(parts, "REMOVE "+strings.Join(removeClauses, ", "))
	}
	return strings.Join(parts, " "), values, true
}

// registerPushEndpoint は端末のトークンを SNS に登録し、そのエンドポイントARNを返す。
// 週一バッチはこの ARN 宛に Publish する。
//
// 同じトークンで既にエンドポイントがあると SNS は新規作成せずに既存のものを返すが、
// 属性（CustomUserData など）が違う場合だけはエラーになる。そのエラー文に既存の ARN が
// 入っているので、拾って使い回す。あわせてトークンを入れ直し、有効化もしておく
// （APNs に弾かれた端末は Enabled=false のまま残るため、入れ直さないと二度と届かない）。
func registerPushEndpoint(ctx context.Context, platformArn, userID, token string) (string, error) {
	out, err := snsClient.CreatePlatformEndpoint(ctx, &sns.CreatePlatformEndpointInput{
		PlatformApplicationArn: aws.String(platformArn),
		Token:                  aws.String(token),
		// 誰の端末か分かるようにしておく。SNS 側から辿るときに要る。
		CustomUserData: aws.String(userID),
	})
	if err == nil {
		return aws.ToString(out.EndpointArn), nil
	}

	arn, ok := existingEndpointARN(err)
	if !ok {
		return "", err
	}
	_, err = snsClient.SetEndpointAttributes(ctx, &sns.SetEndpointAttributesInput{
		EndpointArn: aws.String(arn),
		Attributes: map[string]string{
			"Token":   token,
			"Enabled": "true",
		},
	})
	if err != nil {
		return "", err
	}
	return arn, nil
}

// endpointARNPattern は SNS のエラー文に混じるエンドポイントARNを拾う。
// 文面は
//
//	Invalid parameter: Token Reason: Endpoint arn:aws:sns:ap-northeast-1:123:endpoint/APNS/kotozute/xxxx
//	already exists with the same Token, but different attributes.
//
// のような形をしている。
var endpointARNPattern = regexp.MustCompile(`arn:aws[a-zA-Z0-9-]*:sns:[a-zA-Z0-9:/_-]+`)

// existingEndpointARN は「同じトークンのエンドポイントが既にある」エラーから ARN を取り出す。
// それ以外のエラーでは ok=false を返し、呼び出し側がそのまま失敗として扱えるようにする。
func existingEndpointARN(err error) (string, bool) {
	var invalid *snstypes.InvalidParameterException
	if !errors.As(err, &invalid) {
		return "", false
	}
	arn := endpointARNPattern.FindString(aws.ToString(invalid.Message))
	if arn == "" {
		return "", false
	}
	return arn, true
}

// 端末の持ち主。1台の端末は、いちばん最後に登録したアカウントだけのものにする。
//
//	PK=DEVICE#<APNsトークン>  SK=META  userId=<持ち主>
//
// これが無いと、同じ端末で別のアカウントに切り替えたとき、前のアカウントにも
// 宛先が残る。前のアカウント宛ての通知（相手の名前が入る）が次の人の端末に届く。
// トークンで直引きできるので GSI は要らない。
func deviceKey(token string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"PK": &types.AttributeValueMemberS{Value: "DEVICE#" + token},
		"SK": &types.AttributeValueMemberS{Value: "META"},
	}
}

// claimDevice はこの端末を userID のものにする。前の持ち主が別人なら、そちらから宛先を外す。
//
// 外すのは「前の持ち主がまだこのトークンを持っている」ときだけ。
// その人がすでに別の端末に移っていれば、そちらの登録は触らない。
func claimDevice(ctx context.Context, userID, token string) error {
	out, err := dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key:       deviceKey(token),
	})
	if err != nil {
		return err
	}
	if v, ok := out.Item["userId"].(*types.AttributeValueMemberS); ok && v.Value != userID {
		_, err := dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName: aws.String(tableName),
			Key: map[string]types.AttributeValue{
				"PK": &types.AttributeValueMemberS{Value: "USER#" + v.Value},
				"SK": &types.AttributeValueMemberS{Value: "META"},
			},
			UpdateExpression:    aws.String("REMOVE apnsToken, snsEndpointArn"),
			ConditionExpression: aws.String("apnsToken = :token"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":token": &types.AttributeValueMemberS{Value: token},
			},
		})
		var condErr *types.ConditionalCheckFailedException
		if err != nil && !errors.As(err, &condErr) {
			return err
		}
	}
	item := deviceKey(token)
	item["userId"] = &types.AttributeValueMemberS{Value: userID}
	_, err = dynamoClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(tableName),
		Item:      item,
	})
	return err
}

// releaseDevice は、この端末の持ち主が userID なら記録を消す。
// 別の人がすでに持ち主になっていれば触らない。
func releaseDevice(ctx context.Context, userID, token string) error {
	if token == "" {
		return nil
	}
	_, err := dynamoClient.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName:           aws.String(tableName),
		Key:                 deviceKey(token),
		ConditionExpression: aws.String("userId = :me"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":me": &types.AttributeValueMemberS{Value: userID},
		},
	})
	var condErr *types.ConditionalCheckFailedException
	if err != nil && !errors.As(err, &condErr) {
		return err
	}
	return nil
}

// currentToken は、いま userID に登録されているトークンを返す（無ければ空）。
func currentToken(ctx context.Context, userID string) string {
	out, err := dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:            aws.String(tableName),
		Key:                  map[string]types.AttributeValue{"PK": &types.AttributeValueMemberS{Value: "USER#" + userID}, "SK": &types.AttributeValueMemberS{Value: "META"}},
		ProjectionExpression: aws.String("apnsToken"),
	})
	if err != nil {
		return ""
	}
	if v, ok := out.Item["apnsToken"].(*types.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}

func handler(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	userID, ok := userIDFromRequest(request)
	if !ok {
		return errorResponse(http.StatusUnauthorized, "unauthorized"), nil
	}

	var req patchMeRequest
	if err := json.Unmarshal([]byte(request.Body), &req); err != nil {
		return errorResponse(http.StatusBadRequest, "invalid request body"), nil
	}

	// 表示名が送られてきたときだけ検証する。キーが無ければ更新対象ではない。
	if req.DisplayName != nil {
		name, errMsg := validateDisplayName(*req.DisplayName)
		if errMsg != "" {
			return errorResponse(http.StatusBadRequest, errMsg), nil
		}
		req.DisplayName = &name
	}

	// 端末のトークンが来たら SNS に登録して、通知の宛先（エンドポイントARN）を得る。
	// 失敗しても 400/500 にはしない。apnsToken は保存されるし、アプリは起動のたびに
	// トークンを送ってくるので、次の機会に登録し直せる。
	snsEndpointArn := ""
	platformArn := platformApplicationFor(req.ApnsEnvironment)
	if req.ApnsToken != nil && *req.ApnsToken != "" && platformArn != "" {
		arn, err := registerPushEndpoint(ctx, platformArn, userID, *req.ApnsToken)
		if err != nil {
			log.Printf("failed to register push endpoint for user %s: %v", userID, err)
		} else {
			snsEndpointArn = arn
		}
	}

	// 外すときは、消す前にいまのトークンを控えておく（端末の持ち主の記録を消すのに要る）。
	releasing := req.ApnsToken != nil && *req.ApnsToken == ""
	previousToken := ""
	if releasing {
		previousToken = currentToken(ctx, userID)
	}

	// 送られてきた項目だけを更新する UpdateExpression を組み立てる。
	updateExpression, values, ok := buildUpdateExpression(req, snsEndpointArn)
	if !ok {
		return errorResponse(http.StatusBadRequest, "nothing to update"), nil
	}
	// 値を使わない式（REMOVE だけ）に空のマップを渡すと DynamoDB が弾く。
	if len(values) == 0 {
		values = nil
	}

	_, err := dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("USER#%s", userID)},
			"SK": &types.AttributeValueMemberS{Value: "META"},
		},
		UpdateExpression:          aws.String(updateExpression),
		ConditionExpression:       aws.String("attribute_exists(PK)"), // 存在するユーザーだけ更新（無ければ404）
		ExpressionAttributeValues: values,
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return errorResponse(http.StatusNotFound, "user not registered"), nil
		}
		return errorResponse(http.StatusInternalServerError, "failed to update user"), nil
	}

	// 端末の持ち主を付け替える。失敗しても本体の更新は済んでいるので、ログだけ残す
	// （アプリは起動のたびに送り直すので、次の機会にまた付け替わる）。
	if releasing {
		if err := releaseDevice(ctx, userID, previousToken); err != nil {
			log.Printf("failed to release device for user %s: %v", userID, err)
		}
	} else if req.ApnsToken != nil {
		if err := claimDevice(ctx, userID, *req.ApnsToken); err != nil {
			log.Printf("failed to claim device for user %s: %v", userID, err)
		}
	}

	// 更新後の値を返す（送られた項目のみ反映）。
	res := map[string]string{"userId": userID}
	if req.DisplayName != nil {
		res["displayName"] = *req.DisplayName
	}
	if req.ApnsToken != nil {
		res["apnsToken"] = *req.ApnsToken
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
