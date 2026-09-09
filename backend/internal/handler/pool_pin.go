package handler

import (
	"net/http"
	"strconv"
	"strings"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const (
	// poolPinAccountRequestHeader 是内部测试头：下游中继可为 admin 请求强制指定
	// 池内 OpenAI 账号。仅在 gateway.allow_pool_pin_header 开启且调用方为 admin
	// 时生效，其余情况静默忽略。
	poolPinAccountRequestHeader = "X-Pool-Pin-Account"
	// poolAccountResponseHeader 在响应头中标识本次请求实际服务的池账号，
	// 每次选号后重写，failover 换号时以最后一次选中的账号为准。
	poolAccountResponseHeader = "X-Pool-Account"
)

// tryPoolPinnedAccountSelection 实现带门控的 X-Pool-Pin-Account 入站钉选。
//
// 返回 (selection, true) 表示钉选路径接管本请求：selection 非 nil 时调用方
// 必须用它绕过调度器；selection 为 nil 时错误响应已写出，调用方必须直接返回。
// 返回 (nil, false) 表示门未开启/非 admin/未携带头，调用方继续正常调度。
func (h *OpenAIGatewayHandler) tryPoolPinnedAccountSelection(c *gin.Context, reqLog *zap.Logger) (*service.AccountSelectionResult, bool) {
	if h.cfg == nil || !h.cfg.Gateway.AllowPoolPinHeader {
		return nil, false
	}
	raw := strings.TrimSpace(c.GetHeader(poolPinAccountRequestHeader))
	if raw == "" {
		return nil, false
	}
	if role, ok := middleware2.GetUserRoleFromContext(c); !ok || role != service.RoleAdmin {
		return nil, false
	}
	accountID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || accountID <= 0 {
		reqLog.Warn("openai.pool_pin_invalid_account_id", zap.String("header_value", raw))
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Invalid pool pin account")
		return nil, true
	}
	selection, err := h.gatewayService.SelectPinnedAccount(c.Request.Context(), accountID)
	if err != nil {
		reqLog.Warn("openai.pool_pin_account_unavailable",
			zap.Int64("pinned_account_id", accountID),
			zap.Error(err),
		)
		h.errorResponse(c, http.StatusServiceUnavailable, "api_error", "Pinned account is not available")
		return nil, true
	}
	reqLog.Info("openai.pool_pin_applied", zap.Int64("pinned_account_id", selection.Account.ID))
	return selection, true
}

// setPoolAccountAttributionHeader 在每次选号成功后写入响应头归属，放在
// failover 循环内保证重试换号时覆盖为最终服务账号。
func setPoolAccountAttributionHeader(c *gin.Context, accountID int64) {
	if c == nil || accountID <= 0 {
		return
	}
	c.Header(poolAccountResponseHeader, strconv.FormatInt(accountID, 10))
}
