package main

import (
	"context"
	"net/http"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestParseArchiveQuery(t *testing.T) {
	tests := []struct {
		name       string
		params     map[string]string
		wantErr    bool
		wantWhose  string
		wantYear   int
		wantMonth  int
		wantLimit  int
		wantCursor string
	}{
		{
			name:      "最小構成（whose省略→me、limit省略→10）",
			params:    map[string]string{"year": "2026", "month": "6"},
			wantWhose: "me", wantYear: 2026, wantMonth: 6, wantLimit: 10,
		},
		{
			name:      "partner・limit指定",
			params:    map[string]string{"whose": "partner", "year": "2026", "month": "12", "limit": "5"},
			wantWhose: "partner", wantYear: 2026, wantMonth: 12, wantLimit: 5,
		},
		{
			name:      "limitが上限を超えたら丸められる",
			params:    map[string]string{"year": "2026", "month": "1", "limit": "999"},
			wantWhose: "me", wantYear: 2026, wantMonth: 1, wantLimit: maxLimit,
		},
		{name: "whoseが不正", params: map[string]string{"whose": "both", "year": "2026", "month": "6"}, wantErr: true},
		{name: "yearが無い", params: map[string]string{"month": "6"}, wantErr: true},
		{name: "monthが範囲外", params: map[string]string{"year": "2026", "month": "13"}, wantErr: true},
		{name: "limitが不正", params: map[string]string{"year": "2026", "month": "6", "limit": "abc"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, errMsg := parseArchiveQuery(tt.params)
			if tt.wantErr {
				if errMsg == "" {
					t.Fatalf("expected error, got none")
				}
				return
			}
			if errMsg != "" {
				t.Fatalf("unexpected error: %s", errMsg)
			}
			if q.Whose != tt.wantWhose || q.Year != tt.wantYear || q.Month != tt.wantMonth || q.Limit != tt.wantLimit {
				t.Errorf("got %+v", q)
			}
		})
	}
}

func TestCursorRoundTrip(t *testing.T) {
	// LastEvaluatedKey を encode → decode して元に戻ることを確認する。
	lek := map[string]types.AttributeValue{
		"PK": &types.AttributeValueMemberS{Value: "PAIR#p01"},
		"SK": &types.AttributeValueMemberS{Value: "KOTOZUTE#k01#2026-08-08T10:00:00Z#abc"},
	}
	cursor := encodeCursor(lek)
	if cursor == nil {
		t.Fatal("encodeCursor returned nil for non-empty key")
	}

	decoded, err := decodeCursor(*cursor)
	if err != nil {
		t.Fatalf("decodeCursor error: %v", err)
	}
	for _, key := range []string{"PK", "SK"} {
		got, ok := decoded[key].(*types.AttributeValueMemberS)
		want := lek[key].(*types.AttributeValueMemberS)
		if !ok || got.Value != want.Value {
			t.Errorf("%s round-trip mismatch: got %v, want %q", key, decoded[key], want.Value)
		}
	}
}

func TestEncodeCursor_EmptyReturnsNil(t *testing.T) {
	if got := encodeCursor(map[string]types.AttributeValue{}); got != nil {
		t.Errorf("expected nil for empty key, got %v", got)
	}
}

func TestDecodeCursor_Invalid(t *testing.T) {
	if _, err := decodeCursor("!!!not-base64!!!"); err == nil {
		t.Error("expected error for invalid cursor")
	}
}

func TestPartnerUserID(t *testing.T) {
	m := pairMembers{Member1: "k01", Member2: "k02"}
	if got := partnerUserID(m, "k01"); got != "k02" {
		t.Errorf("got %q, want k02", got)
	}
	if got := partnerUserID(m, "k02"); got != "k01" {
		t.Errorf("got %q, want k01", got)
	}
}

func TestHandler_Unauthorized(t *testing.T) {
	request := events.APIGatewayProxyRequest{
		QueryStringParameters: map[string]string{"year": "2026", "month": "6"},
	}
	response, err := handler(context.Background(), request)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected status %d, got %d", http.StatusUnauthorized, response.StatusCode)
	}
}

// SK からことづての id を取り出せること。
// ことづての置き場所を PAIR# から USER# へ移したとき、この関数だけ古い分割数のままで
// 全件が黙って捨てられ、200 で空配列が返っていた（気づきにくいので回帰防止）。
func TestParseArchiveItem(t *testing.T) {
	item, ok := parseArchiveItem(map[string]types.AttributeValue{
		"SK":        &types.AttributeValueMemberS{Value: "KOTOZUTE#2026-08-16T09:23:18Z#abc-123"},
		"body":      &types.AttributeValueMemberS{Value: "ありがとう"},
		"createdAt": &types.AttributeValueMemberS{Value: "2026-08-16T09:23:18Z"},
	})
	if !ok {
		t.Fatal("正常なSKを弾いてしまった")
	}
	if item.ID != "abc-123" || item.Body != "ありがとう" {
		t.Errorf("unexpected item: %+v", item)
	}

	for _, sk := range []string{"META", "KOTOZUTE#only-two"} {
		if _, ok := parseArchiveItem(map[string]types.AttributeValue{
			"SK": &types.AttributeValueMemberS{Value: sk},
		}); ok {
			t.Errorf("形式違いを受け取ってしまった: %s", sk)
		}
	}
}

// from/to は SK の辞書順で比較されるので、書き方が違っても同じ範囲にならないと
// 間のことづてが落ちる。UTC・秒までの形に揃うことを確かめる。
func TestParseArchiveQuery_Range(t *testing.T) {
	tests := []struct {
		name     string
		params   map[string]string
		wantErr  bool
		wantFrom string
		wantTo   string
	}{
		{
			name:     "UTCならそのまま",
			params:   map[string]string{"from": "2026-08-16T09:25:10Z", "to": "2026-08-17T12:00:22Z"},
			wantFrom: "2026-08-16T09:25:10Z", wantTo: "2026-08-17T12:00:22Z",
		},
		{
			name:     "オフセット付きでもUTCに揃う",
			params:   map[string]string{"from": "2026-08-16T18:25:10+09:00", "to": "2026-08-17T21:00:22+09:00"},
			wantFrom: "2026-08-16T09:25:10Z", wantTo: "2026-08-17T12:00:22Z",
		},
		{name: "fromだけ", params: map[string]string{"from": "2026-08-16T09:25:10Z"}, wantErr: true},
		{name: "toだけ", params: map[string]string{"to": "2026-08-17T12:00:22Z"}, wantErr: true},
		{name: "同じ時刻", params: map[string]string{"from": "2026-08-16T09:25:10Z", "to": "2026-08-16T09:25:10Z"}, wantErr: true},
		{name: "逆順", params: map[string]string{"from": "2026-08-17T12:00:22Z", "to": "2026-08-16T09:25:10Z"}, wantErr: true},
		{name: "ゼロ埋めなし", params: map[string]string{"from": "2026-8-16T09:25:10Z", "to": "2026-08-17T12:00:22Z"}, wantErr: true},
		{name: "時刻として読めない", params: map[string]string{"from": "きのう", "to": "きょう"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, errMsg := parseArchiveQuery(tt.params)
			if tt.wantErr {
				if errMsg == "" {
					t.Fatalf("expected error, got none")
				}
				return
			}
			if errMsg != "" {
				t.Fatalf("unexpected error: %s", errMsg)
			}
			if q.From != tt.wantFrom || q.To != tt.wantTo {
				t.Errorf("from/to = %q/%q, want %q/%q", q.From, q.To, tt.wantFrom, tt.wantTo)
			}
			// 期間指定のときは年月を使わない。
			if q.Year != 0 || q.Month != 0 {
				t.Errorf("year/month should stay unset, got %d/%d", q.Year, q.Month)
			}
		})
	}
}
