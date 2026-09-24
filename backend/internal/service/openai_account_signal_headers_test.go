package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 账号级信号头（x-codex-* / x-base-model-*）要透传给下游 relay 做按号降级监控。
// 这里覆盖 relay 流量实际走的 OpenAI 流式路径（首输出暂存头、提交时才写出）。

func TestOpenAIStreamingRelaysAccountSignalHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
	svc := &OpenAIGatewayService{cfg: cfg, responseHeaderFilter: compileResponseHeaderFilter(cfg)}
	body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_sig\"}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_sig\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{
		"X-Codex-Plan-Type":                   []string{"pro"},
		"X-Codex-Active-Limit":                []string{"premium"},
		"X-Codex-Primary-Used-Percent":        []string{"7"},
		"X-Base-Model-Inference-Limit-Name":   []string{"gpt-reserve"},
		"X-Codex-Turn-State":                  []string{"blob-A"},
		"X-Codex-Safety-Buffering-Enabled":    []string{"true"},
		"X-Some-Unrelated-Upstream-Detail":    []string{"secret"},
		"X-Base-Model-Inference-Reset-Second": []string{"604800"},
	}, Body: io.NopCloser(strings.NewReader(body))}

	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 7, Platform: PlatformOpenAI}, time.Now(), "gpt-6-astra", "gpt-6-astra")
	require.NoError(t, err)

	h := rec.Result().Header
	require.Equal(t, []string{"pro"}, h.Values("X-Codex-Plan-Type"))
	require.Equal(t, []string{"premium"}, h.Values("X-Codex-Active-Limit"))
	require.Equal(t, []string{"7"}, h.Values("X-Codex-Primary-Used-Percent"))
	require.Equal(t, []string{"true"}, h.Values("X-Codex-Safety-Buffering-Enabled"))
	require.Equal(t, []string{"gpt-reserve"}, h.Values("X-Base-Model-Inference-Limit-Name"))
	require.Equal(t, []string{"604800"}, h.Values("X-Base-Model-Inference-Reset-Second"))
	// turn-state 仍由专用逻辑回传，且只有一份（通用过滤器不重复写）。
	require.Equal(t, []string{"blob-A"}, h.Values("X-Codex-Turn-State"))
	require.Empty(t, h.Values("X-Some-Unrelated-Upstream-Detail"))
}

func TestOpenAIStreamingAbandonedAttemptDoesNotLeakAccountSignals(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{Gateway: config.GatewayConfig{
		OpenAIFirstOutputTimeoutSeconds: 1,
		MaxLineSize:                     defaultMaxLineSize,
	}}
	svc := &OpenAIGatewayService{cfg: cfg, responseHeaderFilter: compileResponseHeaderFilter(cfg)}
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{
		"X-Codex-Plan-Type":                 []string{"pro"},
		"X-Base-Model-Inference-Limit-Name": []string{"gpt-reserve"},
	}, Body: pr}

	// 首输出超时 → 本次尝试放弃、准备 failover 换号：该账号的信号头不能写给客户端。
	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 7, Platform: PlatformOpenAI}, time.Now(), "gpt-6-astra", "gpt-6-astra")
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Empty(t, rec.Header().Values("X-Codex-Plan-Type"))
	require.Empty(t, rec.Header().Values("X-Base-Model-Inference-Limit-Name"))
}

func TestWriteOpenAIPassthroughResponseHeaders_RelaysAccountSignals(t *testing.T) {
	dst := http.Header{}
	src := http.Header{}
	src.Set("X-Codex-Plan-Type", "pro")
	src.Set("X-Base-Model-Inference-Limit-Name", "gpt-reserve")
	src.Set("X-Codex-Turn-State", "blob-P")

	writeOpenAIPassthroughResponseHeaders(dst, src, responseheaders.CompileHeaderFilter(config.ResponseHeaderConfig{}))
	require.Equal(t, []string{"pro"}, dst.Values("X-Codex-Plan-Type"))
	require.Equal(t, []string{"gpt-reserve"}, dst.Values("X-Base-Model-Inference-Limit-Name"))
	require.Equal(t, []string{"blob-P"}, dst.Values("X-Codex-Turn-State"))
}
