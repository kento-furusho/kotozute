package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func TestHandler_InvalidJSON(t *testing.T) {
	request := events.APIGatewayProxyRequest{Body: "not json"}

	response, err := handler(context.Background(), request)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, response.StatusCode)
	}
}

func TestHandler_EmptyDisplayName(t *testing.T) {
	request := events.APIGatewayProxyRequest{Body: `{"displayName": ""}`}

	response, err := handler(context.Background(), request)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, response.StatusCode)
	}
}

func TestErrorResponse(t *testing.T) {
	response := errorResponse(http.StatusConflict, "already registered")

	if response.StatusCode != http.StatusConflict {
		t.Errorf("expected status %d, got %d", http.StatusConflict, response.StatusCode)
	}

	var body map[string]string
	if err := json.Unmarshal([]byte(response.Body), &body); err != nil {
		t.Fatalf("failed to unmarshal response body: %v", err)
	}
	if body["error"] != "already registered" {
		t.Errorf("expected error message %q, got %q", "already registered", body["error"])
	}
}
