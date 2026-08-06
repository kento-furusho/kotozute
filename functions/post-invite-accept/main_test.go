package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestHandler_MissingToken(t *testing.T) {
	request := events.APIGatewayProxyRequest{PathParameters: map[string]string{}}

	response, err := handler(context.Background(), request)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, response.StatusCode)
	}
}

func TestHandler_InvalidJSONBody(t *testing.T) {
	request := events.APIGatewayProxyRequest{
		PathParameters: map[string]string{"token": "sometoken"},
		Body:           "not json",
	}

	response, err := handler(context.Background(), request)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, response.StatusCode)
	}
}

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
				"ttl": &types.AttributeValueMemberN{Value: "1786924800"}, // 2026-08-17T00:00:00Z
			},
			expected: false,
		},
		{
			name: "expired",
			item: map[string]types.AttributeValue{
				"ttl": &types.AttributeValueMemberN{Value: "1754784000"}, // 2025-08-10T00:00:00Z
			},
			expected: true,
		},
		{
			name:     "missing ttl treated as expired",
			item:     map[string]types.AttributeValue{},
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
	response := errorResponse(http.StatusConflict, "invite has already been used")

	if response.StatusCode != http.StatusConflict {
		t.Errorf("expected status %d, got %d", http.StatusConflict, response.StatusCode)
	}

	var body map[string]string
	if err := json.Unmarshal([]byte(response.Body), &body); err != nil {
		t.Fatalf("failed to unmarshal response body: %v", err)
	}
	if body["error"] != "invite has already been used" {
		t.Errorf("expected error message %q, got %q", "invite has already been used", body["error"])
	}
}
