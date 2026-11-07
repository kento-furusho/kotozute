package main

import (
	"testing"
	"time"
)

func TestWeekStart(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		// 2026-11-07 は土曜。その週の月曜は 11-02。
		{"2026-11-07T03:00:00Z", "2026-11-02"},
		// 日曜の夜（日本時間）はまだ前の週。
		{"2026-11-08T14:59:00Z", "2026-11-02"},
		// 日本時間の月曜 0:00 になった瞬間から次の週（UTC ではまだ日曜）。
		{"2026-11-08T15:00:00Z", "2026-11-09"},
	}
	for _, tt := range tests {
		in, _ := time.Parse(time.RFC3339, tt.in)
		if got := weekStart(in).Format("2006-01-02"); got != tt.want {
			t.Errorf("weekStart(%s) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestAggregate(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-11-07T03:00:00Z")
	rows := []row{
		{PK: "USER#a", SK: "META", CreatedAt: "2026-11-03T00:00:00Z"},
		{PK: "USER#b", SK: "META"}, // 古いユーザー（作成日時なし）
		{PK: "PAIR#p", SK: "META", CreatedAt: "2026-11-04T00:00:00Z"},
		{PK: "USER#a", SK: "KOTOZUTE#2026-11-05T00:00:00Z#1"},
		{PK: "USER#a", SK: "KOTOZUTE#2026-11-06T00:00:00Z#2"},
		{PK: "USER#b", SK: "KOTOZUTE#2026-10-20T00:00:00Z#3"}, // 18日前
		{PK: "USER#b", SK: "KOTOZUTE#2025-01-01T00:00:00Z#4"}, // 範囲外の古い週
		{PK: "USER#a", SK: "FEEDBACK#2026-11-06T00:00:00Z#f"},
		{PK: "INVITE#x", SK: "META"}, // 数えない
		{PK: "DEVICE#t", SK: "META"}, // 数えない
	}

	s := aggregate(rows, now)

	if s.Users != 2 || s.Pairs != 1 || s.Kotozute != 4 || s.Feedback != 1 {
		t.Errorf("totals = users %d pairs %d kotozute %d feedback %d", s.Users, s.Pairs, s.Kotozute, s.Feedback)
	}
	if s.ActiveWriters7d != 1 || s.ActiveWriters30d != 2 {
		t.Errorf("active = %d / %d, want 1 / 2", s.ActiveWriters7d, s.ActiveWriters30d)
	}
	if s.UsersWithoutCreatedAt != 1 || s.PairsWithoutCreatedAt != 0 {
		t.Errorf("without createdAt = %d / %d", s.UsersWithoutCreatedAt, s.PairsWithoutCreatedAt)
	}
	if len(s.Weekly) != weeksToReport {
		t.Fatalf("weekly len = %d", len(s.Weekly))
	}
	last := s.Weekly[len(s.Weekly)-1]
	if last.WeekStart != "2026-11-02" || last.Kotozute != 2 || last.NewUsers != 1 || last.NewPairs != 1 || last.Feedback != 1 {
		t.Errorf("this week = %+v", last)
	}
	// 古い週は箱がないので、どこにも足されない。
	sum := 0
	for _, w := range s.Weekly {
		sum += w.Kotozute
	}
	if sum != 3 {
		t.Errorf("weekly kotozute sum = %d, want 3", sum)
	}
	// 古い順に並ぶ。
	if s.Weekly[0].WeekStart >= last.WeekStart {
		t.Errorf("weekly not ascending: %s .. %s", s.Weekly[0].WeekStart, last.WeekStart)
	}
}
