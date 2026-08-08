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
	got, err := computeNextOpenAt(5, "20:00", "Asia/Tokyo", now)
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
