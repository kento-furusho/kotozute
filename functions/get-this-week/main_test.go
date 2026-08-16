package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestBuildThisWeekResponse(t *testing.T) {
	mine := []kotozuteItem{{ID: "id1", Body: "私の本文", CreatedAt: "2026-08-01T09:00:00Z"}}
	theirs := []kotozuteItem{{ID: "id2", Body: "相手の本文", CreatedAt: "2026-08-01T10:00:00Z"}}

	testCases := []struct {
		name string
		// 読むサイクル（直前に閉じた分）／書くサイクル（いま書いている分）
		myPrevious            []kotozuteItem
		myCurrent             []kotozuteItem
		partnerPrevious       []kotozuteItem
		partnerCurrent        []kotozuteItem
		expectedCanSeePartner bool
		expectedOpenStatus    string
		expectedHasWritten    bool
		expectPartnerBody     bool
	}{
		{
			name:                  "書くサイクルに自分が書いた・相手はオープン済み → 相手のことづてを見せる",
			myCurrent:             mine,
			partnerPrevious:       theirs,
			expectedCanSeePartner: true,
			expectedOpenStatus:    "opened",
			expectedHasWritten:    false,
			expectPartnerBody:     true,
		},
		{
			// 通知文「今週のことづて、オープンしました」を受け取る人。
			// 書くサイクルはまだ0枚でも、その週ちゃんと書いたので読める。
			name:                  "読むサイクルに自分が書いた → 書くサイクル0枚でもそのまま読める",
			myPrevious:            mine,
			partnerPrevious:       theirs,
			expectedCanSeePartner: true,
			expectedOpenStatus:    "opened",
			expectedHasWritten:    false,
			expectPartnerBody:     true,
		},
		{
			// 通知文「あなたが1つ書くと、○○のことづても見られます」を受け取る人。
			name:                  "どちらのサイクルにも自分が書いていない → ロック（本文は見せない）",
			partnerPrevious:       theirs,
			expectedCanSeePartner: false,
			expectedOpenStatus:    "opened",
			expectedHasWritten:    false,
			expectPartnerBody:     false,
		},
		{
			name:                  "自分1枚以上・読むサイクルに相手なし → オープン前の待ち",
			myCurrent:             mine,
			partnerCurrent:        theirs,
			expectedCanSeePartner: false,
			expectedOpenStatus:    "before",
			expectedHasWritten:    true,
			expectPartnerBody:     false,
		},
		{
			name:                  "自分0枚・読むサイクルに相手なし → 通常ホーム",
			partnerCurrent:        theirs,
			expectedCanSeePartner: false,
			expectedOpenStatus:    "before",
			expectedHasWritten:    true,
			expectPartnerBody:     false,
		},
		{
			name:                  "相手が書くサイクルに0枚 → 気配を出さない",
			myCurrent:             mine,
			expectedCanSeePartner: false,
			expectedOpenStatus:    "before",
			expectedHasWritten:    false,
			expectPartnerBody:     false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			res := buildThisWeekResponse(
				tc.myPrevious, tc.myCurrent,
				tc.partnerPrevious, tc.partnerCurrent,
				"2026-08-07T20:00",
			)

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
				if len(res.LastCycle.Partner) == 0 {
					t.Fatalf("expected LastCycle.Partner to be populated, got empty")
				}
			} else {
				if len(res.LastCycle.Partner) != 0 {
					t.Errorf("expected LastCycle.Partner to be empty (no body leak), got %+v", res.LastCycle.Partner)
				}
			}
		})
	}
}

// myKotozute（「今週のことづて」）に入るのは書くサイクルの分だけ。
// 読むサイクルの自分の分は lastCycle.mine に分けて返す。
func TestBuildThisWeekResponse_MyKotozuteIsCurrentCycleOnly(t *testing.T) {
	previous := []kotozuteItem{{ID: "old", Body: "先週の私", CreatedAt: "2026-08-01T09:00:00Z"}}
	current := []kotozuteItem{{ID: "new", Body: "今週の私", CreatedAt: "2026-08-08T09:00:00Z"}}

	res := buildThisWeekResponse(previous, current, nil, nil, "2026-08-14T20:00")

	if len(res.MyKotozute) != 1 || res.MyKotozute[0].ID != "new" {
		t.Errorf("MyKotozute = %+v, want only the current cycle item", res.MyKotozute)
	}
}

// オープン直後でも相手のことづてが返ること。
// 区切りを1つしか持たなかった頃は、オープンした瞬間に対象が範囲外へ出て
// この状態に到達できなかった（回帰防止）。
func TestBuildThisWeekResponse_ReachableRightAfterOpen(t *testing.T) {
	// オープン直後：書くサイクルは双方まだ空。読むサイクルに双方の分がある。
	myPrevious := []kotozuteItem{{ID: "m", Body: "私の本文", CreatedAt: "2026-08-01T09:00:00Z"}}
	partnerPrevious := []kotozuteItem{{ID: "p", Body: "相手の本文", CreatedAt: "2026-08-01T10:00:00Z"}}

	res := buildThisWeekResponse(myPrevious, nil, partnerPrevious, nil, "2026-08-14T20:00")

	if !res.CanSeePartner {
		t.Fatal("オープン直後に相手のことづてが見られない")
	}
	if len(res.LastCycle.Partner) != 1 {
		t.Errorf("LastCycle.Partner = %+v, want 1 item", res.LastCycle.Partner)
	}
}

// 直前のサイクルの自分の分は、次のオープンまで手元で読める（アーカイブ行きにしない）。
func TestBuildThisWeekResponse_LastCycleMineIsAlwaysReturned(t *testing.T) {
	myPrevious := []kotozuteItem{{ID: "m", Body: "先週の私", CreatedAt: "2026-08-01T09:00:00Z"}}

	t.Run("相手が解禁されていても", func(t *testing.T) {
		partnerPrevious := []kotozuteItem{{ID: "p", Body: "先週の相手", CreatedAt: "2026-08-01T10:00:00Z"}}
		res := buildThisWeekResponse(myPrevious, nil, partnerPrevious, nil, "2026-08-14T20:00")
		if len(res.LastCycle.Mine) != 1 {
			t.Errorf("LastCycle.Mine = %+v, want 1 item", res.LastCycle.Mine)
		}
	})

	t.Run("ロック中でも自分の分は見られる", func(t *testing.T) {
		// 自分は書いていないが相手は書いた、というロック状態を作る。
		partnerPrevious := []kotozuteItem{{ID: "p", Body: "先週の相手", CreatedAt: "2026-08-01T10:00:00Z"}}
		res := buildThisWeekResponse(nil, nil, partnerPrevious, nil, "2026-08-14T20:00")
		if res.CanSeePartner {
			t.Fatal("ロック状態を作れていない")
		}
		if len(res.LastCycle.Partner) != 0 {
			t.Errorf("相手の本文が漏れている: %+v", res.LastCycle.Partner)
		}
	})
}

func TestBuildThisWeekResponse_PartnerBodyNotSerializedWhenLocked(t *testing.T) {
	partnerPrevious := []kotozuteItem{{ID: "id3", Body: "秘密の本文", CreatedAt: "2026-08-01T10:00:00Z"}}
	res := buildThisWeekResponse(nil, nil, partnerPrevious, nil, "2026-08-07T20:00")

	body, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("failed to marshal response: %v", err)
	}
	if got := string(body); contains(got, "秘密の本文") {
		t.Errorf("serialized response leaked partner body: %s", got)
	}
	if contains(string(body), `"partner"`) {
		t.Errorf("expected lastCycle.partner field to be omitted entirely, got %s", body)
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
		"SK":        &types.AttributeValueMemberS{Value: "KOTOZUTE#2026-08-01T09:00:00Z#uuid-1"},
		"body":      &types.AttributeValueMemberS{Value: "ありがとう"},
		"createdAt": &types.AttributeValueMemberS{Value: "2026-08-01T09:00:00Z"},
	}
	item, err := parseKotozuteItem(valid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if item.ID != "uuid-1" || item.Body != "ありがとう" || item.CreatedAt != "2026-08-01T09:00:00Z" {
		t.Errorf("unexpected parsed item: %+v", item)
	}

	testCases := []struct {
		name string
		av   map[string]types.AttributeValue
	}{
		{
			name: "SK format missing parts",
			av: map[string]types.AttributeValue{
				"SK":        &types.AttributeValueMemberS{Value: "KOTOZUTE#only-two"},
				"body":      &types.AttributeValueMemberS{Value: "x"},
				"createdAt": &types.AttributeValueMemberS{Value: "2026-08-01T09:00:00Z"},
			},
		},
		{
			name: "body wrong type",
			av: map[string]types.AttributeValue{
				"SK":        &types.AttributeValueMemberS{Value: "KOTOZUTE#2026-08-01T09:00:00Z#uuid-1"},
				"body":      &types.AttributeValueMemberBOOL{Value: true},
				"createdAt": &types.AttributeValueMemberS{Value: "2026-08-01T09:00:00Z"},
			},
		},
		{
			name: "createdAt missing",
			av: map[string]types.AttributeValue{
				"SK":   &types.AttributeValueMemberS{Value: "KOTOZUTE#2026-08-01T09:00:00Z#uuid-1"},
				"body": &types.AttributeValueMemberS{Value: "x"},
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
		if meta.CycleStartedAt != defaultCycleStartedAt {
			t.Errorf("CycleStartedAt = %q, want %q", meta.CycleStartedAt, defaultCycleStartedAt)
		}
		if meta.PreviousCycleStartedAt != defaultCycleStartedAt {
			t.Errorf("PreviousCycleStartedAt = %q, want %q", meta.PreviousCycleStartedAt, defaultCycleStartedAt)
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
			"cycleStartedAt":         &types.AttributeValueMemberS{Value: "2026-07-25T20:00:00Z"},
			"previousCycleStartedAt": &types.AttributeValueMemberS{Value: "2026-07-18T20:00:00Z"},
			"openDayOfWeek":          &types.AttributeValueMemberN{Value: "3"},
			"openTime":               &types.AttributeValueMemberS{Value: "21:30"},
			"timezone":               &types.AttributeValueMemberS{Value: "UTC"},
		}
		meta := buildPairMetaFromItem(item)
		if meta.CycleStartedAt != "2026-07-25T20:00:00Z" {
			t.Errorf("CycleStartedAt = %q, want %q", meta.CycleStartedAt, "2026-07-25T20:00:00Z")
		}
		if meta.PreviousCycleStartedAt != "2026-07-18T20:00:00Z" {
			t.Errorf("PreviousCycleStartedAt = %q, want %q", meta.PreviousCycleStartedAt, "2026-07-18T20:00:00Z")
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
