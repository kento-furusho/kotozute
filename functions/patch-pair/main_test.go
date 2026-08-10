package main

import (
	"context"
	"net/http"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func intPtr(n int) *int       { return &n }
func strPtr(s string) *string { return &s }

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

func TestBuildPairUpdate(t *testing.T) {
	tests := []struct {
		name        string
		req         patchPairRequest
		wantErrMsg  string // "" は成功期待
		wantClauses int    // 成功時に期待する SET 句の数
	}{
		{
			name:        "曜日と時刻を更新",
			req:         patchPairRequest{OpenDayOfWeek: intPtr(3), OpenTime: strPtr("21:30")},
			wantErrMsg:  "",
			wantClauses: 2,
		},
		{
			name:       "曜日が範囲外(7)",
			req:        patchPairRequest{OpenDayOfWeek: intPtr(7)},
			wantErrMsg: "openDayOfWeek must be between 0 and 6",
		},
		{
			name:       "曜日が負",
			req:        patchPairRequest{OpenDayOfWeek: intPtr(-1)},
			wantErrMsg: "openDayOfWeek must be between 0 and 6",
		},
		{
			name:       "時刻の形式が不正(25:99)",
			req:        patchPairRequest{OpenTime: strPtr("25:99")},
			wantErrMsg: "openTime must be in HH:MM format",
		},
		{
			name:       "タイムゾーンが不正",
			req:        patchPairRequest{Timezone: strPtr("Mars/Olympus")},
			wantErrMsg: "timezone is not a valid IANA timezone",
		},
		{
			name:       "何も無し",
			req:        patchPairRequest{},
			wantErrMsg: "nothing to update",
		},
		{
			name:        "有効なタイムゾーン",
			req:         patchPairRequest{Timezone: strPtr("Asia/Tokyo")},
			wantErrMsg:  "",
			wantClauses: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expr, values, errMsg := buildPairUpdate(tt.req)
			if errMsg != tt.wantErrMsg {
				t.Fatalf("errMsg = %q, want %q", errMsg, tt.wantErrMsg)
			}
			if tt.wantErrMsg != "" {
				return
			}
			if len(values) != tt.wantClauses {
				t.Errorf("len(values) = %d, want %d", len(values), tt.wantClauses)
			}
			if expr == "" {
				t.Errorf("expr should not be empty on success")
			}
		})
	}
}

func TestIsValidHHMM(t *testing.T) {
	valid := []string{"00:00", "23:59", "09:05", "20:00"}
	invalid := []string{"24:00", "23:60", "9:05", "20:0", "2000", "aa:bb", "20:00:00"}
	for _, s := range valid {
		if !isValidHHMM(s) {
			t.Errorf("isValidHHMM(%q) = false, want true", s)
		}
	}
	for _, s := range invalid {
		if isValidHHMM(s) {
			t.Errorf("isValidHHMM(%q) = true, want false", s)
		}
	}
}

func TestHandler_Unauthorized(t *testing.T) {
	request := events.APIGatewayProxyRequest{Body: `{"openTime":"21:00"}`}
	response, err := handler(context.Background(), request)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected status %d, got %d", http.StatusUnauthorized, response.StatusCode)
	}
}

func TestHandler_InvalidValueReturns400(t *testing.T) {
	// 認証は通るが値が不正 → DB に触れる前に 400。
	request := authedRequest(`{"openDayOfWeek": 9}`)
	response, err := handler(context.Background(), request)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, response.StatusCode)
	}
}
