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

// TestNextOpenMoment は次のオープンの決め方。weekly-open / get-me / get-pair / get-this-week で
// 同じ表を流し、アプリに出る「次のオープン」と実際に開く日がずれないことを確かめる。
func TestNextOpenMoment(t *testing.T) {
	// 2026-08-03 は月曜。オープンは金曜 20:00（日本時間）。
	now := mustTime(t, "2026-08-03T09:00:00+09:00")
	tests := []struct {
		name           string
		cycleStartedAt string
		intervalWeeks  int
		want           string
	}{
		{"周期の属性が無い古いペアは毎週", "2026-07-31T11:00:00Z", 0, "2026-08-07T20:00"},
		{"毎週：前回の金曜の次の金曜", "2026-07-31T11:00:00Z", 1, "2026-08-07T20:00"},
		{"2週ごと：前回から7日の金曜は見送り、14日の金曜", "2026-07-31T11:00:00Z", 2, "2026-08-14T20:00"},
		{"4週ごと：28日後の金曜", "2026-07-31T11:00:00Z", 4, "2026-08-28T20:00"},
		// 前回のオープンが前の曜日（日曜）で、曜日を金曜に変えた直後。
		// 金曜まで5日 ≥ 3.5日なので、その金曜に開く。
		{"曜日を変えても前回から十分あいていればすぐの金曜", "2026-08-02T11:00:00Z", 1, "2026-08-07T20:00"},
		// 前回のオープンが木曜。金曜まで1日 < 3.5日なので、次の金曜に回す。
		{"前回から短すぎる金曜は見送る", "2026-08-06T11:00:00Z", 1, "2026-08-14T20:00"},
		{"ペア成立直後（遠い過去）は次の金曜", "1970-01-01T00:00:00Z", 2, "2026-08-07T20:00"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := nextOpenMoment(5, "20:00", "Asia/Tokyo", tt.cycleStartedAt, tt.intervalWeeks, now)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if s := got.Format("2006-01-02T15:04"); s != tt.want {
				t.Errorf("nextOpenMoment() = %s, want %s", s, tt.want)
			}
		})
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("failed to parse %q: %v", s, err)
	}
	return v
}
