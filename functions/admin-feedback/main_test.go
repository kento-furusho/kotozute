package main

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestToFeedback(t *testing.T) {
	item := map[string]types.AttributeValue{
		"PK":         &types.AttributeValueMemberS{Value: "USER#abc"},
		"SK":         &types.AttributeValueMemberS{Value: "FEEDBACK#2026-11-07T00:00:00Z#1"},
		"body":       &types.AttributeValueMemberS{Value: "色を増やしてほしい"},
		"appVersion": &types.AttributeValueMemberS{Value: "1.0.3 (13)"},
		"createdAt":  &types.AttributeValueMemberS{Value: "2026-11-07T00:00:00Z"},
	}
	got := toFeedback(item)
	want := feedback{UserID: "abc", Body: "色を増やしてほしい", AppVersion: "1.0.3 (13)", CreatedAt: "2026-11-07T00:00:00Z"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestSortNewestFirst(t *testing.T) {
	items := []feedback{
		{CreatedAt: "2026-11-01T00:00:00Z"},
		{CreatedAt: "2026-11-07T00:00:00Z"},
		{CreatedAt: "2026-11-03T00:00:00Z"},
	}
	sortNewestFirst(items)
	if items[0].CreatedAt != "2026-11-07T00:00:00Z" || items[2].CreatedAt != "2026-11-01T00:00:00Z" {
		t.Errorf("not newest first: %+v", items)
	}
}
