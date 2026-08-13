package admin

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
)

// openAIRefreshFailedReason 是 repository 层 OpenAI token 端点失败时的错误 reason
// （openai_oauth_service.go 中固定为 502 + 该 reason，message 内嵌上游原始响应）。
const openAIRefreshFailedReason = "OPENAI_OAUTH_TOKEN_REFRESH_FAILED"

// openAIUpstreamRejection 表示 OpenAI token 端点对刷新/轮换请求的确定性拒绝。
type openAIUpstreamRejection struct {
	status  int    // 上游 HTTP 状态码（仅 400/401 视为确定性拒绝）
	code    string // 上游 error.code，如 invalid_refresh_token；可能为空
	message string // 上游 error.message；可能为空
}

func (r *openAIUpstreamRejection) errorMessage() string {
	detail := r.code
	if r.message != "" {
		if detail != "" {
			detail += ": "
		}
		detail += r.message
	}
	if detail == "" {
		detail = fmt.Sprintf("HTTP %d", r.status)
	}
	return fmt.Sprintf("OpenAI rejected the refresh token (%s). The stored refresh token is no longer valid; re-authorize this account.", detail)
}

var (
	openAIUpstreamStatusRe  = regexp.MustCompile(`token refresh failed: status (\d{3})`)
	openAIUpstreamCodeRe    = regexp.MustCompile(`"code"\s*:\s*"([^"]*)"`)
	openAIUpstreamMessageRe = regexp.MustCompile(`"message"\s*:\s*"([^"]*)"`)
)

// parseOpenAIUpstreamRejection 从 OPENAI_OAUTH_TOKEN_REFRESH_FAILED 的 message
// （形如 "token refresh failed: status 401, body: {...}"）解析上游拒绝细节。
// 仅当上游状态码为 400/401（凭证类确定性失败）时返回非 nil；
// 上游 5xx、网络错误等瞬态失败返回 nil，由调用方按原错误透传。
//
// 之所以需要它：这类失败经 ErrorFrom 会以 502 返回，而 5xx 响应经过
// Cloudflare 时会被其品牌错误页替换，管理员在面板里看不到真正的失败原因
// （2026-08-13 生产上排查 invalid_refresh_token 时实际踩到）。
func parseOpenAIUpstreamRejection(message string) *openAIUpstreamRejection {
	m := openAIUpstreamStatusRe.FindStringSubmatch(message)
	if m == nil {
		return nil
	}
	status, err := strconv.Atoi(m[1])
	if err != nil || (status != http.StatusBadRequest && status != http.StatusUnauthorized) {
		return nil
	}
	r := &openAIUpstreamRejection{status: status}
	if cm := openAIUpstreamCodeRe.FindStringSubmatch(message); cm != nil {
		r.code = cm[1]
	}
	if mm := openAIUpstreamMessageRe.FindStringSubmatch(message); mm != nil {
		r.message = mm[1]
	}
	return r
}
