package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type errorCaptureSettingRepo struct {
	service.SettingRepository
	value string
}

func (r *errorCaptureSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	if key == service.SettingKeyOpenAIErrorCaptureRequestEnabled {
		return r.value, nil
	}
	return "", service.ErrSettingNotFound
}

func (r *errorCaptureSettingRepo) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{}, nil
}

// Set 是 no-op：NewOpsService 启动时会对缺失的 runtime log config 做默认回填。
func (r *errorCaptureSettingRepo) Set(context.Context, string, string) error {
	return nil
}

// 上游流内错误 + 抓包开关：验证错误路径落库的 entry 携带脱敏 headers 与截断 body。
func TestLogOpsStreamError_RequestCapture(t *testing.T) {
	newCtx := func() (*gin.Context, *httptest.ResponseRecorder) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
		c.Request.Header.Set("Authorization", "Bearer sk-client-secret")
		c.Request.Header.Set("X-Api-Key", "sk-client-key")
		c.Request.Header.Set("Cookie", "session=secret")
		c.Request.Header.Set("X-Client-Request-Id", "client-req-1")
		c.Request.Header.Set("User-Agent", "codex_cli_rs/0.147.0")
		c.Set(opsModelKey, "gpt-5.1")
		service.MarkOpsStreamFailure(
			c,
			"upstream_error",
			service.OpenAIUpstreamHTTP2StreamErrorCode,
			"Upstream HTTP/2 stream failed",
			http.StatusBadGateway,
		)
		return c, rec
	}

	t.Run("switch off writes nothing", func(t *testing.T) {
		setupOpsErrorLogTestQueue(t, 4)
		c, _ := newCtx()
		c.Set(opsRequestBodyCaptureKey, []byte(`{"model":"gpt-5.1","input":"hello"}`))

		ops := service.NewOpsService(nil, &errorCaptureSettingRepo{}, nil, nil, nil, nil, nil, nil, nil, nil, nil)
		logOpsStreamError(c, ops, http.StatusOK)

		job := <-opsErrorLogQueue
		require.NotNil(t, job.entry)
		require.Empty(t, job.entry.RequestHeaders)
		require.Empty(t, job.entry.RequestBody)
	})

	t.Run("switch on captures redacted headers and truncated body", func(t *testing.T) {
		setupOpsErrorLogTestQueue(t, 4)
		c, _ := newCtx()
		bigBody := []byte(`{"model":"gpt-5.1","input":"` + strings.Repeat("x", service.OpsErrorCaptureBodyMaxBytes) + `"}`)
		c.Set(opsRequestBodyCaptureKey, bigBody)

		ops := service.NewOpsService(nil, &errorCaptureSettingRepo{value: "true"}, nil, nil, nil, nil, nil, nil, nil, nil, nil)
		logOpsStreamError(c, ops, http.StatusOK)

		job := <-opsErrorLogQueue
		require.NotNil(t, job.entry)

		require.NotEmpty(t, job.entry.RequestHeaders)
		require.NotContains(t, job.entry.RequestHeaders, "sk-client-secret")
		require.NotContains(t, job.entry.RequestHeaders, "sk-client-key")
		require.NotContains(t, job.entry.RequestHeaders, "session=secret")
		require.Contains(t, job.entry.RequestHeaders, `"Authorization":"***"`)
		require.Contains(t, job.entry.RequestHeaders, `"X-Client-Request-Id":"client-req-1"`)
		require.Contains(t, job.entry.RequestHeaders, "codex_cli_rs/0.147.0")

		require.NotEmpty(t, job.entry.RequestBody)
		require.LessOrEqual(t, len(job.entry.RequestBody), service.OpsErrorCaptureBodyMaxBytes)
		require.True(t, strings.HasPrefix(job.entry.RequestBody, `{"model":"gpt-5.1"`))
	})

	t.Run("no stashed body keeps request body empty", func(t *testing.T) {
		setupOpsErrorLogTestQueue(t, 4)
		c, _ := newCtx()

		ops := service.NewOpsService(nil, &errorCaptureSettingRepo{value: "true"}, nil, nil, nil, nil, nil, nil, nil, nil, nil)
		logOpsStreamError(c, ops, http.StatusOK)

		job := <-opsErrorLogQueue
		require.NotNil(t, job.entry)
		require.NotEmpty(t, job.entry.RequestHeaders)
		require.Empty(t, job.entry.RequestBody)
	})
}

// 非上游错误（无 upstream 上下文、非流内错误路径）即使开关打开也不抓包。
func TestApplyOpsErrorRequestCapture_SkipsNonUpstreamErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	c.Request.Header.Set("Authorization", "Bearer sk-secret")
	c.Set(opsRequestBodyCaptureKey, []byte(`{"model":"gpt-5.1"}`))

	ops := service.NewOpsService(nil, &errorCaptureSettingRepo{value: "true"}, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	entry := &service.OpsInsertErrorLogInput{}
	applyOpsErrorRequestCapture(c, ops, entry, false)
	require.Empty(t, entry.RequestHeaders)
	require.Empty(t, entry.RequestBody)

	// 有上游上下文（failover 尝试事件）时应抓包。
	status := http.StatusBadRequest
	entry = &service.OpsInsertErrorLogInput{UpstreamStatusCode: &status}
	applyOpsErrorRequestCapture(c, ops, entry, false)
	require.NotEmpty(t, entry.RequestHeaders)
	require.Equal(t, `{"model":"gpt-5.1"}`, entry.RequestBody)
}
