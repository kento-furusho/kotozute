package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

// authedRequest は「Cognito認証を通ったリクエスト」を組み立てるテスト用ヘルパー。
func authedRequest(body string) events.APIGatewayProxyRequest {
	return events.APIGatewayProxyRequest{
		Body: body,
		RequestContext: events.APIGatewayProxyRequestContext{
			Authorizer: map[string]interface{}{
				"claims": map[string]interface{}{"sub": "k01"},
			},
		},
	}
}

func TestHandler_Unauthorized(t *testing.T) {
	// claims の無いリクエスト（トークン無し）は 401 で弾く。
	request := events.APIGatewayProxyRequest{Body: `{"body": "ありがとう"}`}

	response, err := handler(context.Background(), request)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected status %d, got %d", http.StatusUnauthorized, response.StatusCode)
	}
}

func TestHandler_InvalidJSON(t *testing.T) {
	request := authedRequest("not json")

	response, err := handler(context.Background(), request)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, response.StatusCode)
	}
}

func TestHandler_EmptyBody(t *testing.T) {
	request := authedRequest(`{"body": ""}`)

	response, err := handler(context.Background(), request)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, response.StatusCode)
	}
}

func TestErrorResponse(t *testing.T) {
	response := errorResponse(http.StatusBadRequest, "bad request")

	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, response.StatusCode)
	}

	var body map[string]string
	if err := json.Unmarshal([]byte(response.Body), &body); err != nil {
		t.Fatalf("failed to unmarshal response body: %v", err)
	}
	if body["error"] != "bad request" {
		t.Errorf("expected error message %q, got %q", "bad request", body["error"])
	}
}
