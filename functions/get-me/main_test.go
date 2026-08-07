package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestUserIDFromRequest(t *testing.T) {
	// テーブルドリブンテスト：入力と期待値の組を表にして、同じ検証を全パターンに回す。
	tests := []struct {
		name       string
		authorizer map[string]interface{}
		wantID     string
		wantOK     bool
	}{
		{
			name:       "有効なclaimsからsubを取り出せる",
			authorizer: map[string]interface{}{"claims": map[string]interface{}{"sub": "user-123"}},
			wantID:     "user-123",
			wantOK:     true,
		},
		{
			name:       "authorizerが空なら失敗",
			authorizer: map[string]interface{}{},
			wantID:     "",
			wantOK:     false,
		},
		{
			name:       "claimsはあるがsubが無いなら失敗",
			authorizer: map[string]interface{}{"claims": map[string]interface{}{"email": "a@example.com"}},
			wantID:     "",
			wantOK:     false,
		},
		{
			name:       "subが空文字なら失敗",
			authorizer: map[string]interface{}{"claims": map[string]interface{}{"sub": ""}},
			wantID:     "",
			wantOK:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := events.APIGatewayProxyRequest{
				RequestContext: events.APIGatewayProxyRequestContext{
					Authorizer: tt.authorizer,
				},
			}
			gotID, gotOK := userIDFromRequest(request)
			if gotID != tt.wantID || gotOK != tt.wantOK {
				t.Errorf("userIDFromRequest() = (%q, %v), want (%q, %v)", gotID, gotOK, tt.wantID, tt.wantOK)
			}
		})
	}
}

func TestBuildPairMetaFromItem(t *testing.T) {
	t.Run("empty item returns defaults", func(t *testing.T) {
		meta := buildPairMetaFromItem(map[string]types.AttributeValue{})
		if meta.OpenDayOfWeek != 5 || meta.OpenTime != "20:00" || meta.Timezone != "Asia/Tokyo" {
			t.Errorf("unexpected default meta: %+v", meta)
		}
	})

	t.Run("real data overrides defaults", func(t *testing.T) {
		item := map[string]types.AttributeValue{
			"openDayOfWeek": &types.AttributeValueMemberN{Value: "3"},
			"openTime":      &types.AttributeValueMemberS{Value: "21:30"},
			"timezone":      &types.AttributeValueMemberS{Value: "UTC"},
		}
		meta := buildPairMetaFromItem(item)
		if meta.OpenDayOfWeek != 3 || meta.OpenTime != "21:30" || meta.Timezone != "UTC" {
			t.Errorf("unexpected meta: %+v", meta)
		}
	})
}

func TestComputeNextOpenAt(t *testing.T) {
	meta := pairMeta{OpenDayOfWeek: 5, OpenTime: "20:00", Timezone: "Asia/Tokyo"}
	now, err := time.Parse(time.RFC3339, "2026-08-03T09:00:00+09:00")
	if err != nil {
		t.Fatalf("failed to parse now: %v", err)
	}
	got, err := computeNextOpenAt(meta, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "2026-08-07T20:00" {
		t.Errorf("computeNextOpenAt() = %q, want %q", got, "2026-08-07T20:00")
	}
}

func TestErrorResponse(t *testing.T) {
	response := errorResponse(http.StatusNotFound, "user not registered")

	if response.StatusCode != http.StatusNotFound {
		t.Errorf("expected status %d, got %d", http.StatusNotFound, response.StatusCode)
	}

	var body map[string]string
	if err := json.Unmarshal([]byte(response.Body), &body); err != nil {
		t.Fatalf("failed to unmarshal response body: %v", err)
	}
	if body["error"] != "user not registered" {
		t.Errorf("expected error message %q, got %q", "user not registered", body["error"])
	}
}
