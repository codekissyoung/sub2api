package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func newPoolPinTestHandler(allowPin bool, gatewayService *service.OpenAIGatewayService) *OpenAIGatewayHandler {
	cfg := &config.Config{}
	cfg.Gateway.AllowPoolPinHeader = allowPin
	return &OpenAIGatewayHandler{
		gatewayService: gatewayService,
		cfg:            cfg,
	}
}

func newPoolPinTestContext(t *testing.T, role string, setRole bool, pinHeader string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if pinHeader != "" {
		req.Header.Set(poolPinAccountRequestHeader, pinHeader)
	}
	c.Request = req
	if setRole {
		c.Set(string(middleware2.ContextKeyUserRole), role)
	}
	return c, rec
}

func TestTryPoolPinnedAccountSelectionGate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		allowPin  bool
		role      string
		setRole   bool
		pinHeader string
	}{
		{name: "flag disabled ignores header", allowPin: false, role: service.RoleAdmin, setRole: true, pinHeader: "42"},
		{name: "header absent", allowPin: true, role: service.RoleAdmin, setRole: true},
		{name: "non admin role silently ignored", allowPin: true, role: service.RoleUser, setRole: true, pinHeader: "42"},
		{name: "role missing silently ignored", allowPin: true, setRole: false, pinHeader: "42"},
		{name: "blank header value ignored", allowPin: true, role: service.RoleAdmin, setRole: true, pinHeader: "  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPoolPinTestHandler(tc.allowPin, nil)
			c, rec := newPoolPinTestContext(t, tc.role, tc.setRole, tc.pinHeader)

			selection, handled := h.tryPoolPinnedAccountSelection(c, zap.NewNop())
			require.False(t, handled, "gate must fall through to normal scheduling")
			require.Nil(t, selection)
			require.Empty(t, rec.Body.String(), "no error response may be written when the pin is ignored")
		})
	}
}

func TestTryPoolPinnedAccountSelectionRejectsInvalidID(t *testing.T) {
	for _, raw := range []string{"abc", "0", "-3", "1.5"} {
		t.Run(raw, func(t *testing.T) {
			h := newPoolPinTestHandler(true, nil)
			c, rec := newPoolPinTestContext(t, service.RoleAdmin, true, raw)

			selection, handled := h.tryPoolPinnedAccountSelection(c, zap.NewNop())
			require.True(t, handled, "pin path owns the request once the gate passes")
			require.Nil(t, selection)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Contains(t, rec.Body.String(), "invalid_request_error")
		})
	}
}

func TestTryPoolPinnedAccountSelectionUnavailableAccountIs503(t *testing.T) {
	// 真实 service + nil accountRepo：GetByID 不可达即视为不可用，验证 handler
	// 对钉选不可用的确定性 503 映射（绝不回退调度器）。
	gatewayService := service.NewOpenAIGatewayService(
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	h := newPoolPinTestHandler(true, gatewayService)
	c, rec := newPoolPinTestContext(t, service.RoleAdmin, true, "42")

	selection, handled := h.tryPoolPinnedAccountSelection(c, zap.NewNop())
	require.True(t, handled)
	require.Nil(t, selection)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "api_error")
}

func TestSetPoolAccountAttributionHeader(t *testing.T) {
	c, rec := newPoolPinTestContext(t, "", false, "")

	setPoolAccountAttributionHeader(c, 123)
	require.Equal(t, "123", rec.Header().Get(poolAccountResponseHeader))

	// failover 重试换号时覆盖为最新选中的账号。
	setPoolAccountAttributionHeader(c, 456)
	require.Equal(t, "456", rec.Header().Get(poolAccountResponseHeader))

	setPoolAccountAttributionHeader(c, 0)
	require.Equal(t, "456", rec.Header().Get(poolAccountResponseHeader), "invalid ids must not clear the attribution header")
}
