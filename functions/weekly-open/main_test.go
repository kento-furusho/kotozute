package main

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// jst は Asia/Tokyo。tzdata を埋め込んでいるのでホスト環境に依存せず読める。
func jst(t *testing.T) *time.Location {
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("failed to load JST: %v", err)
	}
	return loc
}

func TestShouldOpen(t *testing.T) {
	loc := jst(t)
	// 2026-08-07 は金曜日（Weekday=5）。
	friday2000 := time.Date(2026, 8, 7, 20, 0, 0, 0, loc)
	friday1959 := time.Date(2026, 8, 7, 19, 59, 0, 0, loc)
	saturday := time.Date(2026, 8, 8, 12, 0, 0, 0, loc)

	base := pairMeta{
		OpenDayOfWeek: 5, // 金曜
		OpenTime:      "20:00",
		Timezone:      "Asia/Tokyo",
		LastOpenedAt:  "2026-07-31T20:00:00Z", // 先週オープン済み
	}

	tests := []struct {
		name string
		pair pairMeta
		now  time.Time
		want bool
	}{
		{
			name: "金曜20:00ちょうど・未オープン → オープンする",
			pair: base,
			now:  friday2000,
			want: true,
		},
		{
			name: "金曜19:59・まだ時刻前 → しない",
			pair: base,
			now:  friday1959,
			want: false,
		},
		{
			name: "土曜（オープン曜日でない） → しない",
			pair: base,
			now:  saturday,
			want: false,
		},
		{
			name: "金曜20:00だが今サイクル既にオープン済み → しない",
			pair: func() pairMeta { p := base; p.LastOpenedAt = "2026-08-07T20:00:00+09:00"; return p }(),
			now:  friday2000,
			want: false,
		},
		{
			name: "openTimeが不正 → しない",
			pair: func() pairMeta { p := base; p.OpenTime = "99:99"; return p }(),
			now:  friday2000,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldOpen(tt.pair, tt.now); got != tt.want {
				t.Errorf("shouldOpen() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseHHMM(t *testing.T) {
	h, m, err := parseHHMM("20:05")
	if err != nil || h != 20 || m != 5 {
		t.Errorf("parseHHMM(20:05) = %d,%d,%v", h, m, err)
	}
	if _, _, err := parseHHMM("bad"); err == nil {
		t.Error("expected error for bad input")
	}
}

func TestBuildPairMeta(t *testing.T) {
	item := map[string]types.AttributeValue{
		"PK":            &types.AttributeValueMemberS{Value: "PAIR#p01"},
		"member1":       &types.AttributeValueMemberS{Value: "k01"},
		"member2":       &types.AttributeValueMemberS{Value: "k02"},
		"openDayOfWeek": &types.AttributeValueMemberN{Value: "5"},
		"openTime":      &types.AttributeValueMemberS{Value: "20:00"},
		"timezone":      &types.AttributeValueMemberS{Value: "Asia/Tokyo"},
		"lastOpenedAt":  &types.AttributeValueMemberS{Value: "2026-07-31T20:00:00Z"},
	}
	p := buildPairMeta(item)
	if p.PairID != "p01" || p.Member1 != "k01" || p.Member2 != "k02" || p.OpenDayOfWeek != 5 {
		t.Errorf("unexpected pairMeta: %+v", p)
	}
}
