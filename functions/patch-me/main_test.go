package main

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"
)

// strPtr は文字列のポインタを作るテスト用ヘルパー（*string のリテラルが書けないため）。
func strPtr(s string) *string { return &s }

func authedRequest(body string) events.APIGatewayProxyRequest {
	return events.APIGatewayProxyRequest{
		Body: body,
		RequestContext: events.APIGatewayProxyRequestContext{
			Authorizer: map[string]interface{}{
				"claims": map[string]interface{}{"sub": "k01"},
			},
		},
	}
}

func TestBuildUpdateExpression(t *testing.T) {
	tests := []struct {
		name         string
		req          patchMeRequest
		wantOK       bool
		arn          string
		wantExpr     string
		wantValueLen int
	}{
		{
			name:         "displayNameのみ",
			req:          patchMeRequest{DisplayName: strPtr("けんと")},
			wantOK:       true,
			wantExpr:     "SET displayName = :displayName",
			wantValueLen: 1,
		},
		{
			name:         "apnsTokenのみ",
			req:          patchMeRequest{ApnsToken: strPtr("tok123")},
			wantOK:       true,
			wantExpr:     "SET apnsToken = :apnsToken",
			wantValueLen: 1,
		},
		{
			name:         "両方",
			req:          patchMeRequest{DisplayName: strPtr("けんと"), ApnsToken: strPtr("tok123")},
			wantOK:       true,
			wantExpr:     "SET displayName = :displayName, apnsToken = :apnsToken",
			wantValueLen: 2,
		},
		{
			// サインアウト。空文字を SET すると宛先が残り、前のアカウント宛ての通知が届き続けた。
			name:         "空のトークンは端末の登録を外す",
			req:          patchMeRequest{ApnsToken: strPtr("")},
			wantOK:       true,
			wantExpr:     "REMOVE apnsToken, snsEndpointArn",
			wantValueLen: 0,
		},
		{
			name:         "表示名の変更と端末の解除を同時に",
			req:          patchMeRequest{DisplayName: strPtr("けんと"), ApnsToken: strPtr("")},
			wantOK:       true,
			wantExpr:     "SET displayName = :displayName REMOVE apnsToken, snsEndpointArn",
			wantValueLen: 1,
		},
		{
			name:   "どちらも無し（更新対象なし）",
			req:    patchMeRequest{},
			wantOK: false,
		},
		{
			name:         "端末の登録に成功したらエンドポイントARNも保存する",
			req:          patchMeRequest{ApnsToken: strPtr("tok123")},
			arn:          "arn:aws:sns:ap-northeast-1:123:endpoint/APNS/kotozute/abc",
			wantOK:       true,
			wantExpr:     "SET apnsToken = :apnsToken, snsEndpointArn = :snsEndpointArn",
			wantValueLen: 2,
		},
		{
			name:         "登録に失敗してもトークンは保存する",
			req:          patchMeRequest{ApnsToken: strPtr("tok123")},
			arn:          "",
			wantOK:       true,
			wantExpr:     "SET apnsToken = :apnsToken",
			wantValueLen: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expr, values, ok := buildUpdateExpression(tt.req, tt.arn)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if expr != tt.wantExpr {
				t.Errorf("expr = %q, want %q", expr, tt.wantExpr)
			}
			if len(values) != tt.wantValueLen {
				t.Errorf("len(values) = %d, want %d", len(values), tt.wantValueLen)
			}
		})
	}
}

func TestHandler_Unauthorized(t *testing.T) {
	request := events.APIGatewayProxyRequest{Body: `{"displayName":"けんと"}`}
	response, err := handler(context.Background(), request)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected status %d, got %d", http.StatusUnauthorized, response.StatusCode)
	}
}

func TestHandler_InvalidJSON(t *testing.T) {
	request := authedRequest("not json")
	response, err := handler(context.Background(), request)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, response.StatusCode)
	}
}

func TestHandler_NothingToUpdate(t *testing.T) {
	request := authedRequest(`{}`)
	response, err := handler(context.Background(), request)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, response.StatusCode)
	}
}

// 同じトークンで属性違いのエンドポイントが既にあると、SNS は作成せずエラーを返す。
// そのエラー文から既存の ARN を拾えないと、端末が永遠に登録できない。
func TestExistingEndpointARN(t *testing.T) {
	const arn = "arn:aws:sns:ap-northeast-1:123456789012:endpoint/APNS/kotozute/9b7c1d2e-0000-1111-2222-333344445555"

	tests := []struct {
		name   string
		err    error
		want   string
		wantOK bool
	}{
		{
			name: "既存エンドポイントのエラーからARNを拾う",
			err: &snstypes.InvalidParameterException{
				Message: aws.String("Invalid parameter: Token Reason: Endpoint " + arn +
					" already exists with the same Token, but different attributes."),
			},
			want: arn, wantOK: true,
		},
		{
			name:   "別のInvalidParameter（ARNが無い）は拾わない",
			err:    &snstypes.InvalidParameterException{Message: aws.String("Invalid parameter: Token")},
			wantOK: false,
		},
		{
			name:   "そもそも別のエラー",
			err:    errors.New("network unreachable"),
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := existingEndpointARN(tt.err)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("arn = %q, want %q", got, tt.want)
			}
		})
	}
}

// 空を通すと、相手の画面の「◯◯からのことづて」やロック画面の文言が崩れる。
// 日本語はバイト数だとすぐ上限に当たるので、文字数で数えていることも確かめる。
func TestValidateDisplayName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "ふつうの名前", input: "けんと", want: "けんと"},
		{name: "前後の空白は落とす", input: "  けんと  ", want: "けんと"},
		{name: "全角の空白も落とす", input: "　けんと　", want: "けんと"},
		{name: "20文字ちょうどは通る", input: "あいうえおかきくけこさしすせそたちつてと", want: "あいうえおかきくけこさしすせそたちつてと"},
		{name: "空", input: "", wantErr: true},
		{name: "空白だけ", input: "   ", wantErr: true},
		{name: "全角の空白だけ", input: "　　", wantErr: true},
		{name: "21文字は弾く", input: "あいうえおかきくけこさしすせそたちつてとな", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, errMsg := validateDisplayName(tt.input)
			if tt.wantErr {
				if errMsg == "" {
					t.Fatalf("expected error, got none (got %q)", got)
				}
				return
			}
			if errMsg != "" {
				t.Fatalf("unexpected error: %s", errMsg)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// 環境を取り違えると APNs に拒否され、エンドポイントが無効化されて
// 二度と届かなくなる。省略時にどちらへ倒れるかも含めて押さえる。
func TestPlatformApplicationFor(t *testing.T) {
	platformApplicationArn = "arn:aws:sns:ap-northeast-1:1:app/APNS/kotozute"
	platformApplicationArnSandbox = "arn:aws:sns:ap-northeast-1:1:app/APNS_SANDBOX/kotozute"
	defer func() {
		platformApplicationArn = ""
		platformApplicationArnSandbox = ""
	}()

	tests := []struct {
		name string
		env  *string
		want string
	}{
		{name: "sandbox は開発用", env: strPtr("sandbox"), want: platformApplicationArnSandbox},
		{name: "production は配布用", env: strPtr("production"), want: platformApplicationArn},
		// 配れば大半が配布ビルドなので、迷ったらそちらに倒す。
		{name: "省略は配布用", env: nil, want: platformApplicationArn},
		{name: "知らない値も配布用", env: strPtr("whatever"), want: platformApplicationArn},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := platformApplicationFor(tt.env); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
