package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestBuildThisWeekResponse(t *testing.T) {
	written := []kotozuteItem{{ID: "id1", Body: "私の本文", Opened: false, CreatedAt: "2026-08-01T09:00:00Z"}}
	partnerOpened := []kotozuteItem{{ID: "id2", Body: "相手の本文", Opened: true, CreatedAt: "2026-08-01T10:00:00Z"}}
	partnerNotOpened := []kotozuteItem{{ID: "id3", Body: "相手の本文2", Opened: false, CreatedAt: "2026-08-01T10:00:00Z"}}

	testCases := []struct {
		name                  string
		myItems               []kotozuteItem
		partnerItems          []kotozuteItem
		expectedCanSeePartner bool
		expectedOpenStatus    string
		expectedHasWritten    bool
		expectPartnerBody     bool
	}{
		{
			name:                  "自分1枚以上・相手opened → 相手のことづてを見せる",
			myItems:               written,
			partnerItems:          partnerOpened,
			expectedCanSeePartner: true,
			expectedOpenStatus:    "opened",
			expectedHasWritten:    true,
			expectPartnerBody:     true,
		},
		{
			name:                  "自分1枚以上・相手opened=false → オープン前の待ち",
			myItems:               written,
			partnerItems:          partnerNotOpened,
			expectedCanSeePartner: false,
			expectedOpenStatus:    "before",
			expectedHasWritten:    true,
			expectPartnerBody:     false,
		},
		{
			name:                  "自分0枚・相手opened → ロック（本文は見せない）",
			myItems:               nil,
			partnerItems:          partnerOpened,
			expectedCanSeePartner: false,
			expectedOpenStatus:    "opened",
			expectedHasWritten:    true,
			expectPartnerBody:     false,
		},
		{
			name:                  "自分0枚・相手opened=false → 通常ホーム",
			myItems:               nil,
			partnerItems:          partnerNotOpened,
			expectedCanSeePartner: false,
			expectedOpenStatus:    "before",
			expectedHasWritten:    true,
			expectPartnerBody:     false,
		},
		{
			name:                  "相手0枚 → 気配を出さない（partnerHasWritten=false）",
			myItems:               written,
			partnerItems:          nil,
			expectedCanSeePartner: false,
			expectedOpenStatus:    "before",
			expectedHasWritten:    false,
			expectPartnerBody:     false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			res := buildThisWeekResponse(tc.myItems, tc.partnerItems, "2026-08-07T20:00")

			if res.CanSeePartner != tc.expectedCanSeePartner {
				t.Errorf("CanSeePartner = %v, want %v", res.CanSeePartner, tc.expectedCanSeePartner)
			}
			if res.OpenStatus != tc.expectedOpenStatus {
				t.Errorf("OpenStatus = %q, want %q", res.OpenStatus, tc.expectedOpenStatus)
			}
			if res.PartnerHasWritten != tc.expectedHasWritten {
				t.Errorf("PartnerHasWritten = %v, want %v", res.PartnerHasWritten, tc.expectedHasWritten)
			}

			if tc.expectPartnerBody {
				if len(res.PartnerKotozute) == 0 {
					t.Fatalf("expected PartnerKotozute to be populated, got empty")
				}
			} else {
				if len(res.PartnerKotozute) != 0 {
					t.Errorf("expected PartnerKotozute to be empty (no body leak), got %+v", res.PartnerKotozute)
				}
			}
		})
	}
}

func TestBuildThisWeekResponse_PartnerBodyNotSerializedWhenLocked(t *testing.T) {
	partnerNotOpened := []kotozuteItem{{ID: "id3", Body: "秘密の本文", Opened: false, CreatedAt: "2026-08-01T10:00:00Z"}}
	res := buildThisWeekResponse(nil, partnerNotOpened, "2026-08-07T20:00")

	body, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("failed to marshal response: %v", err)
	}
	if got := string(body); contains(got, "秘密の本文") {
		t.Errorf("serialized response leaked partner body: %s", got)
	}
	if contains(string(body), "partnerKotozute") {
		t.Errorf("expected partnerKotozute field to be omitted entirely, got %s", body)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i+len(substr) <= len(s); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}

func TestParseKotozuteItem(t *testing.T) {
	valid := map[string]types.AttributeValue{
		"SK":        &types.AttributeValueMemberS{Value: "KOTOZUTE#k01#2026-08-01T09:00:00Z#uuid-1"},
		"body":      &types.AttributeValueMemberS{Value: "ありがとう"},
		"opened":    &types.AttributeValueMemberBOOL{Value: true},
		"createdAt": &types.AttributeValueMemberS{Value: "2026-08-01T09:00:00Z"},
	}
	item, err := parseKotozuteItem(valid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if item.ID != "uuid-1" || item.Body != "ありがとう" || !item.Opened || item.CreatedAt != "2026-08-01T09:00:00Z" {
		t.Errorf("unexpected parsed item: %+v", item)
	}

	testCases := []struct {
		name string
		av   map[string]types.AttributeValue
	}{
		{
			name: "SK format missing parts",
			av: map[string]types.AttributeValue{
				"SK":        &types.AttributeValueMemberS{Value: "KOTOZUTE#k01"},
				"body":      &types.AttributeValueMemberS{Value: "x"},
				"opened":    &types.AttributeValueMemberBOOL{Value: false},
				"createdAt": &types.AttributeValueMemberS{Value: "2026-08-01T09:00:00Z"},
			},
		},
		{
			name: "body wrong type",
			av: map[string]types.AttributeValue{
				"SK":        &types.AttributeValueMemberS{Value: "KOTOZUTE#k01#2026-08-01T09:00:00Z#uuid-1"},
				"body":      &types.AttributeValueMemberBOOL{Value: true},
				"opened":    &types.AttributeValueMemberBOOL{Value: false},
				"createdAt": &types.AttributeValueMemberS{Value: "2026-08-01T09:00:00Z"},
			},
		},
		{
			name: "opened missing",
			av: map[string]types.AttributeValue{
				"SK":        &types.AttributeValueMemberS{Value: "KOTOZUTE#k01#2026-08-01T09:00:00Z#uuid-1"},
				"body":      &types.AttributeValueMemberS{Value: "x"},
				"createdAt": &types.AttributeValueMemberS{Value: "2026-08-01T09:00:00Z"},
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseKotozuteItem(tc.av); err == nil {
				t.Errorf("expected error, got nil")
			}
		})
	}
}

func TestBuildPairMetaFromItem(t *testing.T) {
	t.Run("empty item returns defaults", func(t *testing.T) {
		meta := buildPairMetaFromItem(map[string]types.AttributeValue{})
		if meta.LastOpenedAt != defaultLastOpenedAt {
			t.Errorf("LastOpenedAt = %q, want %q", meta.LastOpenedAt, defaultLastOpenedAt)
		}
		if meta.OpenDayOfWeek != defaultOpenDayOfWeek {
			t.Errorf("OpenDayOfWeek = %d, want %d", meta.OpenDayOfWeek, defaultOpenDayOfWeek)
		}
		if meta.OpenTime != defaultOpenTime {
			t.Errorf("OpenTime = %q, want %q", meta.OpenTime, defaultOpenTime)
		}
		if meta.Timezone != defaultTimezone {
			t.Errorf("Timezone = %q, want %q", meta.Timezone, defaultTimezone)
		}
	})

	t.Run("real data overrides defaults", func(t *testing.T) {
		item := map[string]types.AttributeValue{
			"lastOpenedAt":  &types.AttributeValueMemberS{Value: "2026-07-25T20:00:00Z"},
			"openDayOfWeek": &types.AttributeValueMemberN{Value: "3"},
			"openTime":      &types.AttributeValueMemberS{Value: "21:30"},
			"timezone":      &types.AttributeValueMemberS{Value: "UTC"},
		}
		meta := buildPairMetaFromItem(item)
		if meta.LastOpenedAt != "2026-07-25T20:00:00Z" {
			t.Errorf("LastOpenedAt = %q, want %q", meta.LastOpenedAt, "2026-07-25T20:00:00Z")
		}
		if meta.OpenDayOfWeek != 3 {
			t.Errorf("OpenDayOfWeek = %d, want 3", meta.OpenDayOfWeek)
		}
		if meta.OpenTime != "21:30" {
			t.Errorf("OpenTime = %q, want %q", meta.OpenTime, "21:30")
		}
		if meta.Timezone != "UTC" {
			t.Errorf("Timezone = %q, want %q", meta.Timezone, "UTC")
		}
	})
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

func TestComputeNextOpenAt(t *testing.T) {
	meta := pairMeta{OpenDayOfWeek: 5, OpenTime: "20:00", Timezone: "Asia/Tokyo"} // 金曜20:00

	testCases := []struct {
		name     string
		now      string // RFC3339 in Asia/Tokyo
		expected string
	}{
		{
			name:     "月曜午前 → 今週金曜20:00",
			now:      "2026-08-03T09:00:00+09:00", // 月曜
			expected: "2026-08-07T20:00",
		},
		{
			name:     "金曜19:59 → 今日20:00",
			now:      "2026-08-07T19:59:00+09:00",
			expected: "2026-08-07T20:00",
		},
		{
			name:     "金曜ちょうど20:00 → 来週に繰り越す",
			now:      "2026-08-07T20:00:00+09:00",
			expected: "2026-08-14T20:00",
		},
		{
			name:     "金曜20:01 → 来週金曜20:00",
			now:      "2026-08-07T20:01:00+09:00",
			expected: "2026-08-14T20:00",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, tc.now)
			if err != nil {
				t.Fatalf("failed to parse now: %v", err)
			}
			got, err := computeNextOpenAt(meta, now)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.expected {
				t.Errorf("computeNextOpenAt() = %q, want %q", got, tc.expected)
			}
		})
	}
}

func TestErrorResponse(t *testing.T) {
	response := errorResponse(http.StatusInternalServerError, "boom")

	if response.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected status %d, got %d", http.StatusInternalServerError, response.StatusCode)
	}

	var body map[string]string
	if err := json.Unmarshal([]byte(response.Body), &body); err != nil {
		t.Fatalf("failed to unmarshal response body: %v", err)
	}
	if body["error"] != "boom" {
		t.Errorf("expected error message %q, got %q", "boom", body["error"])
	}
}
