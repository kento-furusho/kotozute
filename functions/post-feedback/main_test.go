package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

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
	response, err := handler(context.Background(), events.APIGatewayProxyRequest{Body: `{"body":"いいアプリです"}`})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected %d, got %d", http.StatusUnauthorized, response.StatusCode)
	}
}

func TestHandler_InvalidJSON(t *testing.T) {
	response, _ := handler(context.Background(), authedRequest("not json"))
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("expected %d, got %d", http.StatusBadRequest, response.StatusCode)
	}
}

func TestValidateFeedback(t *testing.T) {
	tests := []struct {
		name        string
		req         postFeedbackRequest
		wantBody    string
		wantVersion string
		wantErr     bool
	}{
		{"通常", postFeedbackRequest{Body: "色を増やしてほしい", AppVersion: "1.0.3 (13)"}, "色を増やしてほしい", "1.0.3 (13)", false},
		{"前後の空白を落とす", postFeedbackRequest{Body: "  ありがとう \n"}, "ありがとう", "", false},
		{"空は不可", postFeedbackRequest{Body: ""}, "", "", true},
		{"空白だけも不可", postFeedbackRequest{Body: " \n\t "}, "", "", true},
		{"上限ちょうどは可", postFeedbackRequest{Body: strings.Repeat("あ", maxFeedbackLength)}, strings.Repeat("あ", maxFeedbackLength), "", false},
		{"上限超えは不可", postFeedbackRequest{Body: strings.Repeat("あ", maxFeedbackLength+1)}, "", "", true},
		{"バージョンが長すぎる", postFeedbackRequest{Body: "a", AppVersion: strings.Repeat("9", maxAppVersionLength+1)}, "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, version, errMsg := validateFeedback(tt.req)
			if (errMsg != "") != tt.wantErr {
				t.Fatalf("errMsg = %q, wantErr %v", errMsg, tt.wantErr)
			}
			if body != tt.wantBody || version != tt.wantVersion {
				t.Errorf("got (%q, %q), want (%q, %q)", body, version, tt.wantBody, tt.wantVersion)
			}
		})
	}
}
