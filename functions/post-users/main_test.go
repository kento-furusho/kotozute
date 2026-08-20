package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

// authedRequest は「Cognito認証を通ったリクエスト」を組み立てるテスト用ヘルパー。
// 本番ではAPI Gatewayが詰めてくれる claims を、テストでは自分で用意する。
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

func TestHandler_Unauthorized(t *testing.T) {
	// claims の無いリクエスト（トークン無し）は 401 で弾く。
	request := events.APIGatewayProxyRequest{Body: `{"displayName": "けんと"}`}

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

func TestHandler_EmptyDisplayName(t *testing.T) {
	request := authedRequest(`{"displayName": ""}`)

	response, err := handler(context.Background(), request)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, response.StatusCode)
	}
}

func TestErrorResponse(t *testing.T) {
	response := errorResponse(http.StatusConflict, "already registered")

	if response.StatusCode != http.StatusConflict {
		t.Errorf("expected status %d, got %d", http.StatusConflict, response.StatusCode)
	}

	var body map[string]string
	if err := json.Unmarshal([]byte(response.Body), &body); err != nil {
		t.Fatalf("failed to unmarshal response body: %v", err)
	}
	if body["error"] != "already registered" {
		t.Errorf("expected error message %q, got %q", "already registered", body["error"])
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
