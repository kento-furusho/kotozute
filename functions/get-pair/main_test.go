package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestHandler_Unauthorized(t *testing.T) {
	// claims の無いリクエスト（トークン無し）は 401 で弾く。
	request := events.APIGatewayProxyRequest{}

	response, err := handler(context.Background(), request)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected status %d, got %d", http.StatusUnauthorized, response.StatusCode)
	}
}

func TestPartnerUserID(t *testing.T) {
	tests := []struct {
		name     string
		meta     pairMeta
		myUserID string
		want     string
	}{
		{
			name:     "自分がmember1なら相手はmember2",
			meta:     pairMeta{Member1: "k01", Member2: "k02"},
			myUserID: "k01",
			want:     "k02",
		},
		{
			name:     "自分がmember2なら相手はmember1",
			meta:     pairMeta{Member1: "k01", Member2: "k02"},
			myUserID: "k02",
			want:     "k01",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := partnerUserID(tt.meta, tt.myUserID); got != tt.want {
				t.Errorf("partnerUserID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildPairMetaFromItem(t *testing.T) {
	t.Run("設定が無ければデフォルトで補う", func(t *testing.T) {
		meta := buildPairMetaFromItem(map[string]types.AttributeValue{
			"member1": &types.AttributeValueMemberS{Value: "k01"},
			"member2": &types.AttributeValueMemberS{Value: "k02"},
		})
		if meta.OpenDayOfWeek != 5 || meta.OpenTime != "20:00" || meta.Timezone != "Asia/Tokyo" {
			t.Errorf("unexpected defaults: %+v", meta)
		}
	})

	t.Run("実データで上書きされる", func(t *testing.T) {
		meta := buildPairMetaFromItem(map[string]types.AttributeValue{
			"member1":       &types.AttributeValueMemberS{Value: "k01"},
			"member2":       &types.AttributeValueMemberS{Value: "k02"},
			"openDayOfWeek": &types.AttributeValueMemberN{Value: "3"},
			"openTime":      &types.AttributeValueMemberS{Value: "21:30"},
			"timezone":      &types.AttributeValueMemberS{Value: "UTC"},
			"status":        &types.AttributeValueMemberS{Value: "active"},
		})
		if meta.OpenDayOfWeek != 3 || meta.OpenTime != "21:30" || meta.Timezone != "UTC" || meta.Status != "active" {
			t.Errorf("unexpected meta: %+v", meta)
		}
	})
}

func TestComputeNextOpenAt(t *testing.T) {
	now, err := time.Parse(time.RFC3339, "2026-08-03T09:00:00+09:00")
	if err != nil {
		t.Fatalf("failed to parse now: %v", err)
	}
	got, err := computeNextOpenAt(pairMeta{OpenDayOfWeek: 5, OpenTime: "20:00", Timezone: "Asia/Tokyo"}, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "2026-08-07T20:00" {
		t.Errorf("computeNextOpenAt() = %q, want %q", got, "2026-08-07T20:00")
	}
}

func TestErrorResponse(t *testing.T) {
	response := errorResponse(http.StatusNotFound, "no pair")
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("expected status %d, got %d", http.StatusNotFound, response.StatusCode)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(response.Body), &body); err != nil {
		t.Fatalf("failed to unmarshal response body: %v", err)
	}
	if body["error"] != "no pair" {
		t.Errorf("expected error %q, got %q", "no pair", body["error"])
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
