package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestIsInviteExpired(t *testing.T) {
	now, err := time.Parse(time.RFC3339, "2026-08-10T00:00:00Z")
	if err != nil {
		t.Fatalf("failed to parse now: %v", err)
	}

	testCases := []struct {
		name     string
		item     map[string]types.AttributeValue
		expected bool
	}{
		{
			name: "not expired",
			item: map[string]types.AttributeValue{
				"ttl": &types.AttributeValueMemberN{Value: "1786924800"}, // 2026-08-17T00:00:00Z 相当の未来
			},
			expected: false,
		},
		{
			name: "expired",
			item: map[string]types.AttributeValue{
				"ttl": &types.AttributeValueMemberN{Value: "1754784000"}, // 2025-08-10T00:00:00Z 相当の過去
			},
			expected: true,
		},
		{
			name:     "missing ttl treated as expired",
			item:     map[string]types.AttributeValue{},
			expected: true,
		},
		{
			name: "invalid ttl treated as expired",
			item: map[string]types.AttributeValue{
				"ttl": &types.AttributeValueMemberS{Value: "not-a-number"},
			},
			expected: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := isInviteExpired(tc.item, now)
			if got != tc.expected {
				t.Errorf("isInviteExpired() = %v, want %v", got, tc.expected)
			}
		})
	}
}

func TestErrorResponse(t *testing.T) {
	response := errorResponse(http.StatusNotFound, "invite not found")

	if response.StatusCode != http.StatusNotFound {
		t.Errorf("expected status %d, got %d", http.StatusNotFound, response.StatusCode)
	}

	var body map[string]string
	if err := json.Unmarshal([]byte(response.Body), &body); err != nil {
		t.Fatalf("failed to unmarshal response body: %v", err)
	}
	if body["error"] != "invite not found" {
		t.Errorf("expected error message %q, got %q", "invite not found", body["error"])
	}
}
