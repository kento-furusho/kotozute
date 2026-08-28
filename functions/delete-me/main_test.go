package main

import (
	"context"
	"net/http"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// 相手を取り違えると、消した本人ではなく相手のペアを壊す。
func TestPartnerUserID(t *testing.T) {
	tests := []struct {
		name   string
		item   map[string]types.AttributeValue
		userID string
		want   string
	}{
		{
			name: "member1 が自分なら member2",
			item: map[string]types.AttributeValue{
				"member1": &types.AttributeValueMemberS{Value: "me"},
				"member2": &types.AttributeValueMemberS{Value: "you"},
			},
			userID: "me", want: "you",
		},
		{
			name: "member2 が自分なら member1",
			item: map[string]types.AttributeValue{
				"member1": &types.AttributeValueMemberS{Value: "you"},
				"member2": &types.AttributeValueMemberS{Value: "me"},
			},
			userID: "me", want: "you",
		},
		// 片方しか無いペアは壊れたデータ。相手なしとして扱い、消す側だけ進める。
		{
			name: "自分しかいない",
			item: map[string]types.AttributeValue{
				"member1": &types.AttributeValueMemberS{Value: "me"},
			},
			userID: "me", want: "",
		},
		{name: "空", item: map[string]types.AttributeValue{}, userID: "me", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := partnerUserID(tt.item, tt.userID); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandler_Unauthorized(t *testing.T) {
	res, err := handler(context.Background(), events.APIGatewayProxyRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", res.StatusCode, http.StatusUnauthorized)
	}
}

func TestStringAttr(t *testing.T) {
	item := map[string]types.AttributeValue{
		"pairId": &types.AttributeValueMemberS{Value: " p1 "},
		"n":      &types.AttributeValueMemberN{Value: "1"},
	}
	if got := stringAttr(item, "pairId"); got != "p1" {
		t.Errorf("前後の空白は落とす: got %q", got)
	}
	if got := stringAttr(item, "n"); got != "" {
		t.Errorf("文字列以外は空: got %q", got)
	}
	if got := stringAttr(item, "missing"); got != "" {
		t.Errorf("無い項目は空: got %q", got)
	}
}
