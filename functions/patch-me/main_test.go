package main

import (
	"context"
	"net/http"
	"testing"

	"github.com/aws/aws-lambda-go/events"
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
			name:   "どちらも無し（更新対象なし）",
			req:    patchMeRequest{},
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expr, values, ok := buildUpdateExpression(tt.req)
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
