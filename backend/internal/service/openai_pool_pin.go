package service

import (
	"context"
	"errors"
	"fmt"
)

// ErrPinnedAccountUnavailable 表示钉选账号不可用（不存在、非 OpenAI 平台或
// 非 active 状态）。handler 将其映射为中性的 503，绝不回退到调度器。
var ErrPinnedAccountUnavailable = errors.New("pinned account is not available")

// SelectPinnedAccount 按 admin 钉选加载指定的 OpenAI 池账号，绕过调度器。
//
// 返回的 AccountSelectionResult 携带 WaitPlan（复用 fallback 等待参数），使
// handler 的账号并发准入与正常调度路径一致；不附带利润门，终检/粘性绑定语义
// 与无门调度结果相同。账号不可用时返回包裹 ErrPinnedAccountUnavailable 的
// 错误，调用方必须确定性失败，不得回退调度。
func (s *OpenAIGatewayService) SelectPinnedAccount(ctx context.Context, accountID int64) (*AccountSelectionResult, error) {
	if s == nil || s.accountRepo == nil || accountID <= 0 {
		return nil, ErrPinnedAccountUnavailable
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("%w: lookup account %d: %v", ErrPinnedAccountUnavailable, accountID, err)
	}
	if account == nil || account.Platform != PlatformOpenAI || !account.IsActive() {
		return nil, fmt.Errorf("%w: account %d", ErrPinnedAccountUnavailable, accountID)
	}
	cfg := s.schedulingConfig()
	return &AccountSelectionResult{
		Account: account,
		WaitPlan: &AccountWaitPlan{
			AccountID:      account.ID,
			MaxConcurrency: account.Concurrency,
			Timeout:        cfg.FallbackWaitTimeout,
			MaxWaiting:     cfg.FallbackMaxWaiting,
		},
	}, nil
}
