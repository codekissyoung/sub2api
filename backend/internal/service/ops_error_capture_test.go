package service

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

type errorCaptureSettingRepoStub struct {
	SettingRepository
	value         string
	getValueCalls int
}

func (r *errorCaptureSettingRepoStub) GetValue(_ context.Context, key string) (string, error) {
	r.getValueCalls++
	if key == SettingKeyOpenAIErrorCaptureRequestEnabled {
		return r.value, nil
	}
	return "", ErrSettingNotFound
}

func (r *errorCaptureSettingRepoStub) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{}, nil
}

func TestIsOpenAIErrorCaptureRequestEnabled(t *testing.T) {
	t.Run("default off when setting missing", func(t *testing.T) {
		repo := &errorCaptureSettingRepoStub{value: ""}
		// 空值视为关闭（默认关）。
		ops := &OpsService{settingRepo: repo}
		require.False(t, ops.IsOpenAIErrorCaptureRequestEnabled(context.Background()))
	})

	t.Run("on when true", func(t *testing.T) {
		repo := &errorCaptureSettingRepoStub{value: "true"}
		ops := &OpsService{settingRepo: repo}
		require.True(t, ops.IsOpenAIErrorCaptureRequestEnabled(context.Background()))
	})

	t.Run("off for non-true values", func(t *testing.T) {
		repo := &errorCaptureSettingRepoStub{value: "false"}
		ops := &OpsService{settingRepo: repo}
		require.False(t, ops.IsOpenAIErrorCaptureRequestEnabled(context.Background()))
	})

	t.Run("caches within TTL", func(t *testing.T) {
		repo := &errorCaptureSettingRepoStub{value: "true"}
		ops := &OpsService{settingRepo: repo}
		require.True(t, ops.IsOpenAIErrorCaptureRequestEnabled(context.Background()))
		require.True(t, ops.IsOpenAIErrorCaptureRequestEnabled(context.Background()))
		require.Equal(t, 1, repo.getValueCalls, "TTL 内第二次读取应命中缓存")
	})

	t.Run("nil repo or service stays off", func(t *testing.T) {
		var nilOps *OpsService
		require.False(t, nilOps.IsOpenAIErrorCaptureRequestEnabled(context.Background()))
		ops := &OpsService{}
		require.False(t, ops.IsOpenAIErrorCaptureRequestEnabled(context.Background()))
	})
}

func TestSanitizeOpsErrorCaptureHeaders(t *testing.T) {
	header := http.Header{}
	header.Set("Authorization", "Bearer sk-live-secret")
	header.Set("X-Api-Key", "sk-another-secret")
	header.Set("Cookie", "session=secret")
	header.Set("X-Client-Request-Id", "req-keep-me")
	header.Set("User-Agent", "codex_cli_rs/0.147.0")
	header.Set("Content-Type", "application/json")

	out := SanitizeOpsErrorCaptureHeaders(header)
	require.NotEmpty(t, out)
	require.NotContains(t, out, "sk-live-secret")
	require.NotContains(t, out, "sk-another-secret")
	require.NotContains(t, out, "session=secret")
	require.Contains(t, out, `"Authorization":"***"`)
	require.Contains(t, out, `"X-Api-Key":"***"`)
	require.Contains(t, out, `"Cookie":"***"`)
	require.Contains(t, out, `"X-Client-Request-Id":"req-keep-me"`)
	require.Contains(t, out, `"User-Agent":"codex_cli_rs/0.147.0"`)

	require.Empty(t, SanitizeOpsErrorCaptureHeaders(nil))
}

func TestTruncateOpsErrorCaptureBody(t *testing.T) {
	require.Empty(t, TruncateOpsErrorCaptureBody(nil))

	small := []byte(`{"model":"gpt-5.1"}`)
	require.Equal(t, string(small), TruncateOpsErrorCaptureBody(small))

	big := []byte(strings.Repeat("a", OpsErrorCaptureBodyMaxBytes+100))
	truncated := TruncateOpsErrorCaptureBody(big)
	require.LessOrEqual(t, len(truncated), OpsErrorCaptureBodyMaxBytes)

	// 非法 UTF-8 必须被清洗；截断点落在多字节字符中间时也不能产出非法 UTF-8。
	raw := append([]byte(strings.Repeat("好", OpsErrorCaptureBodyMaxBytes)), 0xff, 0xfe)
	out := TruncateOpsErrorCaptureBody(raw)
	require.LessOrEqual(t, len(out), OpsErrorCaptureBodyMaxBytes)
	require.NotContains(t, out, "\ufffd\ufffd") // 尾部残缺的 0xff 0xfe 应被替换/清除
	require.True(t, utf8.ValidString(out))
}
