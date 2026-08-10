package main

import (
	"context"
	"net/http"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func authedRequest(id, body string) events.APIGatewayProxyRequest {
	return events.APIGatewayProxyRequest{
		Body:           body,
		PathParameters: map[string]string{"id": id},
		RequestContext: events.APIGatewayProxyRequestContext{
			Authorizer: map[string]interface{}{
				"claims": map[string]interface{}{"sub": "k01"},
			},
		},
	}
}

func TestKotozuteIDFromSK(t *testing.T) {
	tests := []struct {
		name string
		sk   string
		want string
	}{
		{
			name: "正常なSK",
			sk:   "KOTOZUTE#k01#2026-08-08T10:00:00Z#abc-123",
			want: "abc-123",
		},
		{
			name: "本文にidが含んでも末尾だけ取る",
			sk:   "KOTOZUTE#k01#2026-08-08T10:00:00Z#uuid-with-hash",
			want: "uuid-with-hash",
		},
		{
			name: "形式が違えば空",
			sk:   "META",
			want: "",
		},
		{
			name: "区切りが足りなければ空",
			sk:   "KOTOZUTE#k01#only-three",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := kotozuteIDFromSK(tt.sk); got != tt.want {
				t.Errorf("kotozuteIDFromSK(%q) = %q, want %q", tt.sk, got, tt.want)
			}
		})
	}
}

func TestHandler_Unauthorized(t *testing.T) {
	request := events.APIGatewayProxyRequest{
		PathParameters: map[string]string{"id": "abc"},
		Body:           `{"body":"新しい本文"}`,
	}
	response, err := handler(context.Background(), request)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected status %d, got %d", http.StatusUnauthorized, response.StatusCode)
	}
}

func TestHandler_MissingID(t *testing.T) {
	request := authedRequest("", `{"body":"新しい本文"}`)
	response, err := handler(context.Background(), request)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, response.StatusCode)
	}
}

func TestHandler_EmptyBody(t *testing.T) {
	request := authedRequest("abc", `{"body":""}`)
	response, err := handler(context.Background(), request)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, response.StatusCode)
	}
}

func TestHandler_InvalidJSON(t *testing.T) {
	request := authedRequest("abc", "not json")
	response, err := handler(context.Background(), request)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, response.StatusCode)
	}
}
