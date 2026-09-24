package responseheaders

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func TestFilterHeadersDisabledUsesDefaultAllowlist(t *testing.T) {
	src := http.Header{}
	src.Add("Content-Type", "application/json")
	src.Add("X-Request-Id", "req-123")
	src.Add("X-Test", "ok")
	src.Add("Connection", "keep-alive")
	src.Add("Content-Length", "123")

	cfg := config.ResponseHeaderConfig{
		Enabled:     false,
		ForceRemove: []string{"x-request-id"},
	}

	filtered := FilterHeaders(src, CompileHeaderFilter(cfg))
	if filtered.Get("Content-Type") != "application/json" {
		t.Fatalf("expected Content-Type passthrough, got %q", filtered.Get("Content-Type"))
	}
	if filtered.Get("X-Request-Id") != "req-123" {
		t.Fatalf("expected X-Request-Id allowed, got %q", filtered.Get("X-Request-Id"))
	}
	if filtered.Get("X-Test") != "" {
		t.Fatalf("expected X-Test removed, got %q", filtered.Get("X-Test"))
	}
	if filtered.Get("Connection") != "" {
		t.Fatalf("expected Connection to be removed, got %q", filtered.Get("Connection"))
	}
	if filtered.Get("Content-Length") != "" {
		t.Fatalf("expected Content-Length to be removed, got %q", filtered.Get("Content-Length"))
	}
}

func TestFilterHeadersAllowsReasoningIncludedByDefault(t *testing.T) {
	src := http.Header{}
	src.Set("X-Reasoning-Included", "1")

	filtered := FilterHeaders(src, CompileHeaderFilter(config.ResponseHeaderConfig{}))
	if got := filtered.Get("X-Reasoning-Included"); got != "1" {
		t.Fatalf("expected X-Reasoning-Included passthrough, got %q", got)
	}
}

func TestFilterHeadersForceRemoveOverridesReasoningIncluded(t *testing.T) {
	src := http.Header{}
	src.Set("X-Reasoning-Included", "1")

	filtered := FilterHeaders(src, CompileHeaderFilter(config.ResponseHeaderConfig{
		Enabled:     true,
		ForceRemove: []string{"x-reasoning-included"},
	}))
	if got := filtered.Get("X-Reasoning-Included"); got != "" {
		t.Fatalf("expected X-Reasoning-Included removal, got %q", got)
	}
}

func TestFilterHeadersEnabledUsesAllowlist(t *testing.T) {
	src := http.Header{}
	src.Add("Content-Type", "application/json")
	src.Add("X-Extra", "ok")
	src.Add("X-Remove", "nope")
	src.Add("X-Blocked", "nope")

	cfg := config.ResponseHeaderConfig{
		Enabled:           true,
		AdditionalAllowed: []string{"x-extra"},
		ForceRemove:       []string{"x-remove"},
	}

	filtered := FilterHeaders(src, CompileHeaderFilter(cfg))
	if filtered.Get("Content-Type") != "application/json" {
		t.Fatalf("expected Content-Type allowed, got %q", filtered.Get("Content-Type"))
	}
	if filtered.Get("X-Extra") != "ok" {
		t.Fatalf("expected X-Extra allowed, got %q", filtered.Get("X-Extra"))
	}
	if filtered.Get("X-Remove") != "" {
		t.Fatalf("expected X-Remove removed, got %q", filtered.Get("X-Remove"))
	}
	if filtered.Get("X-Blocked") != "" {
		t.Fatalf("expected X-Blocked removed, got %q", filtered.Get("X-Blocked"))
	}
}

func TestFilterHeadersAllowsAccountSignalPrefixesByDefault(t *testing.T) {
	src := http.Header{}
	src.Set("X-Codex-Plan-Type", "pro")
	src.Set("X-Codex-Primary-Used-Percent", "12")
	src.Set("X-Codex-Some-Future-Signal", "1")
	src.Set("X-Base-Model-Inference-Limit-Name", "gpt-reserve")
	src.Set("X-Codex-Turn-State", "blob")
	src.Set("X-Codexy", "not-a-prefix-match")
	src.Set("X-Models-Etag", "etag")

	for _, cfg := range []config.ResponseHeaderConfig{{}, {Enabled: false}, {Enabled: true}} {
		filtered := FilterHeaders(src, CompileHeaderFilter(cfg))
		if got := filtered.Get("X-Codex-Plan-Type"); got != "pro" {
			t.Fatalf("expected X-Codex-Plan-Type passthrough, got %q", got)
		}
		if got := filtered.Get("X-Codex-Primary-Used-Percent"); got != "12" {
			t.Fatalf("expected X-Codex-Primary-Used-Percent passthrough, got %q", got)
		}
		if got := filtered.Get("X-Codex-Some-Future-Signal"); got != "1" {
			t.Fatalf("expected unknown x-codex-* header passthrough, got %q", got)
		}
		if got := filtered.Get("X-Base-Model-Inference-Limit-Name"); got != "gpt-reserve" {
			t.Fatalf("expected X-Base-Model-* passthrough, got %q", got)
		}
		// turn-state 由 service 层带溯源专门回传，通用过滤器不放行。
		if got := filtered.Get("X-Codex-Turn-State"); got != "" {
			t.Fatalf("expected X-Codex-Turn-State left to dedicated relay, got %q", got)
		}
		if got := filtered.Get("X-Codexy"); got != "" {
			t.Fatalf("expected X-Codexy removed, got %q", got)
		}
		if got := filtered.Get("X-Models-Etag"); got != "" {
			t.Fatalf("expected X-Models-Etag removed, got %q", got)
		}
	}
}

func TestFilterHeadersForceRemoveOverridesAccountSignalPrefix(t *testing.T) {
	src := http.Header{}
	src.Set("X-Codex-Plan-Type", "pro")
	src.Set("X-Codex-Active-Limit", "premium")

	filtered := FilterHeaders(src, CompileHeaderFilter(config.ResponseHeaderConfig{
		Enabled:     true,
		ForceRemove: []string{"x-codex-plan-type"},
	}))
	if got := filtered.Get("X-Codex-Plan-Type"); got != "" {
		t.Fatalf("expected X-Codex-Plan-Type force-removed, got %q", got)
	}
	if got := filtered.Get("X-Codex-Active-Limit"); got != "premium" {
		t.Fatalf("expected X-Codex-Active-Limit passthrough, got %q", got)
	}
}

func TestWriteFilteredHeadersReplacesStaleAccountSignals(t *testing.T) {
	filter := CompileHeaderFilter(config.ResponseHeaderConfig{})
	dst := http.Header{}
	dst.Set("X-Codex-Turn-State", "managed-elsewhere")
	dst.Set("X-Other", "kept")

	// 第一次尝试（账号 A）
	first := http.Header{}
	first.Set("X-Codex-Primary-Used-Percent", "90")
	first.Set("X-Base-Model-Inference-Limit-Name", "gpt-reserve")
	WriteFilteredHeaders(dst, first, filter)

	// failover 到账号 B：B 没有 x-base-model-*，A 的值不能残留，也不能叠成两个值
	second := http.Header{}
	second.Set("X-Codex-Primary-Used-Percent", "10")
	WriteFilteredHeaders(dst, second, filter)

	if got := dst.Values("X-Codex-Primary-Used-Percent"); len(got) != 1 || got[0] != "10" {
		t.Fatalf("expected only account B usage, got %v", got)
	}
	if got := dst.Get("X-Base-Model-Inference-Limit-Name"); got != "" {
		t.Fatalf("expected stale account A x-base-model-* cleared, got %q", got)
	}
	if got := dst.Get("X-Codex-Turn-State"); got != "managed-elsewhere" {
		t.Fatalf("expected turn-state untouched, got %q", got)
	}
	if got := dst.Get("X-Other"); got != "kept" {
		t.Fatalf("expected unrelated header untouched, got %q", got)
	}
}
