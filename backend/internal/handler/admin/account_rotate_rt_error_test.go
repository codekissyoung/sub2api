package admin

import (
	"strings"
	"testing"
)

func TestParseOpenAIUpstreamRejection(t *testing.T) {
	tests := []struct {
		name        string
		message     string
		wantNil     bool
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{
			name: "401 invalid_refresh_token（生产实录，账号 11）",
			message: `token refresh failed: status 401, body: {
 "error": {
   "message": "Invalid refresh token.",
   "type": "invalid_request_error",
   "param": null,
   "code": "invalid_refresh_token"
 }
}`,
			wantStatus:  401,
			wantCode:    "invalid_refresh_token",
			wantMessage: "Invalid refresh token.",
		},
		{
			name: "401 refresh_token_invalidated（生产实录，账号 24）",
			message: `token refresh failed: status 401, body: {
 "error": {
   "message": "Your session has ended. Please log in again.",
   "type": "invalid_request_error",
   "param": null,
   "code": "refresh_token_invalidated"
 }
}`,
			wantStatus:  401,
			wantCode:    "refresh_token_invalidated",
			wantMessage: "Your session has ended. Please log in again.",
		},
		{
			name:        "400 invalid_grant",
			message:     `token refresh failed: status 400, body: {"error":{"message":"The refresh token is invalid","code":"invalid_grant"}}`,
			wantStatus:  400,
			wantCode:    "invalid_grant",
			wantMessage: "The refresh token is invalid",
		},
		{
			name:        "401 无 code 字段时按状态码兜底",
			message:     `token refresh failed: status 401, body: {"error":"unauthorized"}`,
			wantStatus:  401,
			wantCode:    "",
			wantMessage: "",
		},
		{
			name:    "上游 502 瞬态失败不翻译",
			message: `token refresh failed: status 502, body: <html>cloudflare error</html>`,
			wantNil: true,
		},
		{
			name:    "上游 429 不翻译",
			message: `token refresh failed: status 429, body: {"error":{"code":"rate_limit"}}`,
			wantNil: true,
		},
		{
			name:    "网络错误无 status 段",
			message: `token refresh request failed: connection reset by peer`,
			wantNil: true,
		},
		{
			name:    "空 message",
			message: ``,
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseOpenAIUpstreamRejection(tt.message)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("parseOpenAIUpstreamRejection() = %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("parseOpenAIUpstreamRejection() = nil, want non-nil")
			}
			if got.status != tt.wantStatus {
				t.Errorf("status = %d, want %d", got.status, tt.wantStatus)
			}
			if got.code != tt.wantCode {
				t.Errorf("code = %q, want %q", got.code, tt.wantCode)
			}
			if got.message != tt.wantMessage {
				t.Errorf("message = %q, want %q", got.message, tt.wantMessage)
			}
		})
	}
}

func TestOpenAIUpstreamRejectionErrorMessage(t *testing.T) {
	full := (&openAIUpstreamRejection{status: 401, code: "invalid_refresh_token", message: "Invalid refresh token."}).errorMessage()
	if !strings.Contains(full, "invalid_refresh_token") || !strings.Contains(full, "Invalid refresh token.") {
		t.Errorf("errorMessage() 缺少上游细节: %q", full)
	}
	if !strings.Contains(full, "re-authorize") {
		t.Errorf("errorMessage() 缺少重新授权提示: %q", full)
	}

	fallback := (&openAIUpstreamRejection{status: 401}).errorMessage()
	if !strings.Contains(fallback, "HTTP 401") {
		t.Errorf("无上游细节时应回退到状态码: %q", fallback)
	}
}
