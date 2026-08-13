package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// GetOpenAIPoolEstimate 返回 OpenAI OAuth 号池剩余金额估值汇总
// GET /api/v1/admin/accounts/openai-pool-estimate
func (h *AccountHandler) GetOpenAIPoolEstimate(c *gin.Context) {
	// 复用调度分数过滤池的查询：取全部未删除的 active OpenAI OAuth 账号（不分页）。
	accounts, err := h.adminService.ListAccountsForSchedulerScoreFilter(
		c.Request.Context(),
		service.PlatformOpenAI,
		service.AccountTypeOAuth,
		service.StatusActive,
		"",
		0,
		"",
	)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, service.EstimateOpenAIPoolValue(accounts))
}
