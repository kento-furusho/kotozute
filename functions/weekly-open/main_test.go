package main

import (
	"encoding/json"
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
		OpenDayOfWeek:  5, // 金曜
		OpenTime:       "20:00",
		Timezone:       "Asia/Tokyo",
		CycleStartedAt: "2026-07-31T20:00:00Z", // 先週オープンして始まったサイクル
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
			pair: func() pairMeta { p := base; p.CycleStartedAt = "2026-08-07T20:00:00+09:00"; return p }(),
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

func TestBuildNotifications(t *testing.T) {
	pair := pairMeta{Member1: "k01", Member2: "k02"}

	t.Run("両者ゼロは通知しない", func(t *testing.T) {
		if got := buildNotifications(pair, false, false, "けんと", "あい"); got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})

	// 自分だけ書いた週に「オープンしました」が飛んでいた。開いても何も増えていない。
	t.Run("片方だけ書いた場合、書いた人には通知しない", func(t *testing.T) {
		got := buildNotifications(pair, true, false, "けんと", "あい")
		if len(got) != 1 {
			t.Fatalf("expected 1 notification, got %d: %+v", len(got), got)
		}
		// 書いていない member2 にだけ、相手（けんと）の名前入りで知らせる。
		if got[0].UserID != "k02" || got[0].Message != "あなたが1つ書くと、けんとのことづても見られます" {
			t.Errorf("notification wrong: %+v", got[0])
		}
	})

	t.Run("両者書いた", func(t *testing.T) {
		got := buildNotifications(pair, true, true, "けんと", "あい")
		if len(got) != 2 {
			t.Fatalf("expected 2 notifications, got %d", len(got))
		}
		for _, n := range got {
			if n.Message != "ことづてがオープンしました" {
				t.Errorf("expected open message, got %q", n.Message)
			}
		}
	})
}

func TestBuildPairMeta(t *testing.T) {
	item := map[string]types.AttributeValue{
		"PK":             &types.AttributeValueMemberS{Value: "PAIR#p01"},
		"member1":        &types.AttributeValueMemberS{Value: "k01"},
		"member2":        &types.AttributeValueMemberS{Value: "k02"},
		"openDayOfWeek":  &types.AttributeValueMemberN{Value: "5"},
		"openTime":       &types.AttributeValueMemberS{Value: "20:00"},
		"timezone":       &types.AttributeValueMemberS{Value: "Asia/Tokyo"},
		"cycleStartedAt": &types.AttributeValueMemberS{Value: "2026-07-31T20:00:00Z"},
	}
	p := buildPairMeta(item)
	if p.PairID != "p01" || p.Member1 != "k01" || p.Member2 != "k02" || p.OpenDayOfWeek != 5 {
		t.Errorf("unexpected pairMeta: %+v", p)
	}
	if p.CycleStartedAt != "2026-07-31T20:00:00Z" {
		t.Errorf("CycleStartedAt = %q, want %q", p.CycleStartedAt, "2026-07-31T20:00:00Z")
	}
}

// APNs は aps を含む JSON でないと通知を表示しない。
// 素の文字列を渡すと送信は成功して見えるのに端末には何も出ないので、
// 形が崩れていないかをここで押さえる。
func TestBuildPushPayload(t *testing.T) {
	const message = "ことづてがオープンしました"
	const arn = "arn:aws:sns:ap-northeast-1:123456789012:endpoint/APNS_SANDBOX/kotozute/abc-def"

	raw, err := buildPushPayload(arn, message)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var outer map[string]string
	if err := json.Unmarshal([]byte(raw), &outer); err != nil {
		t.Fatalf("outer is not JSON: %v", err)
	}
	if outer["default"] != message {
		t.Errorf("default = %q, want %q", outer["default"], message)
	}

	// サンドボックスのエンドポイントなら、キーは APNS ではなく APNS_SANDBOX。
	apnsJSON, ok := outer["APNS_SANDBOX"]
	if !ok {
		t.Fatalf("APNS_SANDBOX key is missing: %v", outer)
	}

	var inner struct {
		APS struct {
			Alert string `json:"alert"`
			Sound string `json:"sound"`
		} `json:"aps"`
	}
	if err := json.Unmarshal([]byte(apnsJSON), &inner); err != nil {
		t.Fatalf("inner is not JSON: %v", err)
	}
	if inner.APS.Alert != message {
		t.Errorf("aps.alert = %q, want %q", inner.APS.Alert, message)
	}
	if inner.APS.Sound == "" {
		t.Error("aps.sound is empty")
	}
}

func TestPlatformFromEndpointARN(t *testing.T) {
	tests := []struct {
		name string
		arn  string
		want string
	}{
		{
			name: "サンドボックス",
			arn:  "arn:aws:sns:ap-northeast-1:123:endpoint/APNS_SANDBOX/kotozute/abc",
			want: "APNS_SANDBOX",
		},
		{
			name: "本番",
			arn:  "arn:aws:sns:ap-northeast-1:123:endpoint/APNS/kotozute/abc",
			want: "APNS",
		},
		// 読めないときに空のキーを作ると SNS に弾かれる。本番として扱う。
		{name: "エンドポイントARNではない", arn: "arn:aws:sns:ap-northeast-1:123:topic", want: "APNS"},
		{name: "空", arn: "", want: "APNS"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := platformFromEndpointARN(tt.arn); got != tt.want {
				t.Errorf("platform = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestShouldOpenInterval は周期と、前回からの間隔の判定。
func TestShouldOpenInterval(t *testing.T) {
	// 2026-08-07 は金曜。オープンは金曜 20:00（日本時間）で、その5分後にバッチが走った想定。
	now := mustParse(t, "2026-08-07T20:05:00+09:00")
	base := pairMeta{OpenDayOfWeek: 5, OpenTime: "20:00", Timezone: "Asia/Tokyo"}
	tests := []struct {
		name           string
		cycleStartedAt string
		intervalWeeks  int
		want           bool
	}{
		{"毎週：前回は1週間前", "2026-07-31T11:00:00Z", 1, true},
		{"2週ごと：前回は1週間前なので見送る", "2026-07-31T11:00:00Z", 2, false},
		{"2週ごと：前回は2週間前なので開く", "2026-07-24T11:00:00Z", 2, true},
		{"今日すでに開いている（二重オープン防止）", "2026-08-07T11:00:30Z", 1, false},
		// 以前は、前回（木曜）から1日しか経っていなくても開いていた。
		{"曜日を変えた直後で前回から1日しかない", "2026-08-06T11:00:00Z", 1, false},
		{"ペア成立直後（遠い過去）", "1970-01-01T00:00:00Z", 4, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := base
			p.CycleStartedAt = tt.cycleStartedAt
			p.OpenIntervalWeeks = tt.intervalWeeks
			if got := shouldOpen(p, now); got != tt.want {
				t.Errorf("shouldOpen() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestShouldOpenMatchesNextOpenMoment は、アプリに出る「次のオープン」の時刻にバッチが実際に開くこと、
// その1週間前には開かないことを確かめる。2つの計算が食い違うと、表示と実際がずれる。
func TestShouldOpenMatchesNextOpenMoment(t *testing.T) {
	asOf := mustParse(t, "2026-08-03T09:00:00+09:00")
	for _, weeks := range []int{1, 2, 3, 4} {
		p := pairMeta{OpenDayOfWeek: 5, OpenTime: "20:00", Timezone: "Asia/Tokyo",
			CycleStartedAt: "2026-07-31T11:00:00Z", OpenIntervalWeeks: weeks}
		next, err := nextOpenMoment(p.OpenDayOfWeek, p.OpenTime, p.Timezone, p.CycleStartedAt, weeks, asOf)
		if err != nil {
			t.Fatal(err)
		}
		if !shouldOpen(p, next.Add(5*time.Minute)) {
			t.Errorf("%d週ごと: 次のオープン %s に開かない", weeks, next)
		}
		if weeks > 1 && shouldOpen(p, next.AddDate(0, 0, -7).Add(5*time.Minute)) {
			t.Errorf("%d週ごと: 次のオープンの1週間前に開いてしまう", weeks)
		}
	}
}

func mustParse(t *testing.T, v string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, v)
	if err != nil {
		t.Fatalf("failed to parse %q: %v", v, err)
	}
	return parsed
}
