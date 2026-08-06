package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestErrorResponse(t *testing.T) {
	response := errorResponse(http.StatusConflict, "user is already paired")

	if response.StatusCode != http.StatusConflict {
		t.Errorf("expected status %d, got %d", http.StatusConflict, response.StatusCode)
	}

	var body map[string]string
	if err := json.Unmarshal([]byte(response.Body), &body); err != nil {
		t.Fatalf("failed to unmarshal response body: %v", err)
	}
	if body["error"] != "user is already paired" {
		t.Errorf("expected error message %q, got %q", "user is already paired", body["error"])
	}
}
