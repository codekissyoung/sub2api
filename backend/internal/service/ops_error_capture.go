package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"
)

// 上游报错抓包：开关 openai_error_capture_request_enabled（默认关）打开时，
// 错误路径把脱敏后的客户端请求 headers 与截断的 request body 落入
// ops_error_logs.request_headers / request_body。仅错误路径触发，正常请求零开销。

const (
	// OpsErrorCaptureBodyMaxBytes 是抓包 request body 的截断上限（16KB）。
	OpsErrorCaptureBodyMaxBytes = 16 * 1024
	// opsErrorCaptureHeadersMaxBytes 是抓包 headers JSON 的截断上限。
	opsErrorCaptureHeadersMaxBytes = 8 * 1024

	opsErrorCaptureRequestCacheTTL  = 5 * time.Second
	opsErrorCaptureRequestErrorTTL  = 5 * time.Second
	opsErrorCaptureRequestDBTimeout = 2 * time.Second
)

// opsErrorCaptureSensitiveHeaders 落库前必须打码的请求头（小写）。
// x-client-request-id 保留原值，用于与客户端侧日志关联。
var opsErrorCaptureSensitiveHeaders = map[string]struct{}{
	"authorization":       {},
	"proxy-authorization": {},
	"x-api-key":           {},
	"x-goog-api-key":      {},
	"cookie":              {},
	"set-cookie":          {},
}

type cachedOpsErrorCaptureRequestEnabled struct {
	enabled   bool
	expiresAt int64 // unix nano
}

// IsOpenAIErrorCaptureRequestEnabled 返回上游报错抓包开关，进程内 5s TTL 缓存 +
// singleflight（与 openai_advanced_scheduler_* 的 runtime setting 读法一致）。
// 默认 false：空值/未设置/查询失败均为关，保证开关关闭时零行为变化。
func (s *OpsService) IsOpenAIErrorCaptureRequestEnabled(ctx context.Context) bool {
	if s == nil || s.settingRepo == nil {
		return false
	}
	if cached, ok := s.errorCaptureRequestCache.Load().(*cachedOpsErrorCaptureRequestEnabled); ok && cached != nil {
		if time.Now().UnixNano() < cached.expiresAt {
			return cached.enabled
		}
	}
	result, _, _ := s.errorCaptureRequestSF.Do(SettingKeyOpenAIErrorCaptureRequestEnabled, func() (any, error) {
		if cached, ok := s.errorCaptureRequestCache.Load().(*cachedOpsErrorCaptureRequestEnabled); ok && cached != nil {
			if time.Now().UnixNano() < cached.expiresAt {
				return cached, nil
			}
		}
		if ctx == nil {
			ctx = context.Background()
		}
		dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opsErrorCaptureRequestDBTimeout)
		defer cancel()
		value, err := s.settingRepo.GetValue(dbCtx, SettingKeyOpenAIErrorCaptureRequestEnabled)
		if err != nil && !errors.Is(err, ErrSettingNotFound) {
			slog.Warn("failed to get openai_error_capture_request_enabled setting", "error", err)
			entry := &cachedOpsErrorCaptureRequestEnabled{
				enabled:   false,
				expiresAt: time.Now().Add(opsErrorCaptureRequestErrorTTL).UnixNano(),
			}
			s.errorCaptureRequestCache.Store(entry)
			return entry, nil
		}
		entry := &cachedOpsErrorCaptureRequestEnabled{
			enabled:   err == nil && strings.EqualFold(strings.TrimSpace(value), "true"),
			expiresAt: time.Now().Add(opsErrorCaptureRequestCacheTTL).UnixNano(),
		}
		s.errorCaptureRequestCache.Store(entry)
		return entry, nil
	})
	if entry, ok := result.(*cachedOpsErrorCaptureRequestEnabled); ok && entry != nil {
		return entry.enabled
	}
	return false
}

// SanitizeOpsErrorCaptureHeaders 把客户端请求头序列化为 JSON，敏感头打码为 ***。
// 多值头保留为数组；结果超过上限时截断。
func SanitizeOpsErrorCaptureHeaders(header http.Header) string {
	if len(header) == 0 {
		return ""
	}
	keys := make([]string, 0, len(header))
	for key := range header {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	sanitized := make(map[string]any, len(header))
	for _, key := range keys {
		values := header[key]
		if _, sensitive := opsErrorCaptureSensitiveHeaders[strings.ToLower(strings.TrimSpace(key))]; sensitive {
			sanitized[key] = "***"
			continue
		}
		switch len(values) {
		case 0:
			sanitized[key] = ""
		case 1:
			sanitized[key] = values[0]
		default:
			sanitized[key] = values
		}
	}
	encoded, err := json.Marshal(sanitized)
	if err != nil {
		return ""
	}
	return truncateString(string(encoded), opsErrorCaptureHeadersMaxBytes)
}

// TruncateOpsErrorCaptureBody 截断抓包 request body 到 16KB 并保证合法 UTF-8。
func TruncateOpsErrorCaptureBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	text := strings.ToValidUTF8(string(body), "")
	if len(text) > OpsErrorCaptureBodyMaxBytes {
		// 截断可能切断多字节字符，再过一次 ToValidUTF8 替换尾部残缺 rune。
		text = strings.ToValidUTF8(text[:OpsErrorCaptureBodyMaxBytes], "")
	}
	return text
}
