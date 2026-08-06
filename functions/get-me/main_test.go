package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

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
