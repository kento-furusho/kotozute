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
