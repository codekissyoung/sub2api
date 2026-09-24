package responseheaders

import (
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// defaultAllowed 定义允许透传的响应头白名单
// 注意：以下头部由 Go HTTP 包自动处理，不应手动设置：
//   - content-length: 由 ResponseWriter 根据实际写入数据自动设置
//   - transfer-encoding: 由 HTTP 库根据需要自动添加/移除
//   - connection: 由 HTTP 库管理连接复用
var defaultAllowed = map[string]struct{}{
	"content-type":                   {},
	"content-encoding":               {},
	"content-language":               {},
	"cache-control":                  {},
	"etag":                           {},
	"last-modified":                  {},
	"expires":                        {},
	"vary":                           {},
	"date":                           {},
	"x-request-id":                   {},
	"x-ratelimit-limit-requests":     {},
	"x-ratelimit-limit-tokens":       {},
	"x-ratelimit-remaining-requests": {},
	"x-ratelimit-remaining-tokens":   {},
	"x-ratelimit-reset-requests":     {},
	"x-ratelimit-reset-tokens":       {},
	"retry-after":                    {},
	"location":                       {},
	"www-authenticate":               {},
	// Codex uses this response header to avoid estimating reasoning tokens a
	// second time when upstream usage already includes them.
	"x-reasoning-included": {},
}

// defaultAllowedPrefixes 按前缀放行的响应头（ice）。OpenAI/Codex 在响应头里下发
// 账号级信号——x-codex-*（用量窗口、套餐、active limit、credits、安全缓冲等）、
// x-base-model-*（gpt-reserve 这类预留额度）——下游 relay 靠它们按号监控降级。
// 用前缀而不是逐个列名：上游新加的同族头自动透传，relay 侧才能第一时间发现。
var defaultAllowedPrefixes = []string{"x-codex-", "x-base-model-"}

// prefixExcluded 命中前缀但不走通用透传的头。x-codex-turn-state 由
// service/openai_codex_turn_state.go 带铸造账号溯源显式回传，这里不插手。
var prefixExcluded = map[string]struct{}{
	"x-codex-turn-state": {},
}

// isPrefixAllowed 判断小写头名是否属于按前缀放行的账号级信号头。
func isPrefixAllowed(lower string) bool {
	if _, excluded := prefixExcluded[lower]; excluded {
		return false
	}
	for _, prefix := range defaultAllowedPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// hopByHopHeaders 是跳过的 hop-by-hop 头部，这些头部由 HTTP 库自动处理
var hopByHopHeaders = map[string]struct{}{
	"content-length":    {},
	"transfer-encoding": {},
	"connection":        {},
}

type CompiledHeaderFilter struct {
	allowed     map[string]struct{}
	forceRemove map[string]struct{}
}

var defaultCompiledHeaderFilter = CompileHeaderFilter(config.ResponseHeaderConfig{})

func CompileHeaderFilter(cfg config.ResponseHeaderConfig) *CompiledHeaderFilter {
	allowed := make(map[string]struct{}, len(defaultAllowed)+len(cfg.AdditionalAllowed))
	for key := range defaultAllowed {
		allowed[key] = struct{}{}
	}
	// 关闭时只使用默认白名单，additional/force_remove 不生效
	if cfg.Enabled {
		for _, key := range cfg.AdditionalAllowed {
			normalized := strings.ToLower(strings.TrimSpace(key))
			if normalized == "" {
				continue
			}
			allowed[normalized] = struct{}{}
		}
	}

	forceRemove := map[string]struct{}{}
	if cfg.Enabled {
		forceRemove = make(map[string]struct{}, len(cfg.ForceRemove))
		for _, key := range cfg.ForceRemove {
			normalized := strings.ToLower(strings.TrimSpace(key))
			if normalized == "" {
				continue
			}
			forceRemove[normalized] = struct{}{}
		}
	}

	return &CompiledHeaderFilter{
		allowed:     allowed,
		forceRemove: forceRemove,
	}
}

func FilterHeaders(src http.Header, filter *CompiledHeaderFilter) http.Header {
	if filter == nil {
		filter = defaultCompiledHeaderFilter
	}

	filtered := make(http.Header, len(src))
	for key, values := range src {
		lower := strings.ToLower(key)
		if _, blocked := filter.forceRemove[lower]; blocked {
			continue
		}
		if _, ok := filter.allowed[lower]; !ok && !isPrefixAllowed(lower) {
			continue
		}
		// 跳过 hop-by-hop 头部，这些由 HTTP 库自动处理
		if _, isHopByHop := hopByHopHeaders[lower]; isHopByHop {
			continue
		}
		for _, value := range values {
			filtered.Add(key, value)
		}
	}
	return filtered
}

func WriteFilteredHeaders(dst http.Header, src http.Header, filter *CompiledHeaderFilter) {
	// 前缀放行的是账号级信号：先清掉 dst 上已有的同族头，failover 换号时上一个
	// 账号的值不会残留到最终响应上（与 X-Pool-Account 的最终归属保持一致）。
	for key := range dst {
		if isPrefixAllowed(strings.ToLower(key)) {
			dst.Del(key)
		}
	}
	filtered := FilterHeaders(src, filter)
	for key, values := range filtered {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}
