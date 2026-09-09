package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func newPinnedAccountTestService(accounts []Account) *OpenAIGatewayService {
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.FallbackWaitTimeout = 17 * time.Second
	cfg.Gateway.Scheduling.FallbackMaxWaiting = 23
	return &OpenAIGatewayService{
		accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts},
		cfg:         cfg,
	}
}

func TestSelectPinnedAccountBuildsWaitPlanSelection(t *testing.T) {
	svc := newPinnedAccountTestService([]Account{
		{
			ID:          42,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 7,
		},
	})

	selection, err := svc.SelectPinnedAccount(context.Background(), 42)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(42), selection.Account.ID)
	require.False(t, selection.Acquired, "pinned selection must go through WaitPlan admission")
	require.Nil(t, selection.ReleaseFunc)
	require.False(t, selection.ProfitGateActive(), "pinned selection carries no profit gate")
	require.NotNil(t, selection.WaitPlan)
	require.Equal(t, int64(42), selection.WaitPlan.AccountID)
	require.Equal(t, 7, selection.WaitPlan.MaxConcurrency)
	require.Equal(t, 17*time.Second, selection.WaitPlan.Timeout)
	require.Equal(t, 23, selection.WaitPlan.MaxWaiting)
}

func TestSelectPinnedAccountUsesDefaultsWithoutConfig(t *testing.T) {
	svc := &OpenAIGatewayService{
		accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{
			{ID: 7, Platform: PlatformOpenAI, Status: StatusActive, Concurrency: 3},
		}},
	}

	selection, err := svc.SelectPinnedAccount(context.Background(), 7)
	require.NoError(t, err)
	require.NotNil(t, selection.WaitPlan)
	require.Equal(t, 30*time.Second, selection.WaitPlan.Timeout)
	require.Equal(t, 100, selection.WaitPlan.MaxWaiting)
}

func TestSelectPinnedAccountRejectsUnavailableAccounts(t *testing.T) {
	svc := newPinnedAccountTestService([]Account{
		{ID: 1, Platform: PlatformGrok, Status: StatusActive, Concurrency: 1},
		{ID: 2, Platform: PlatformOpenAI, Status: StatusDisabled, Concurrency: 1},
		{ID: 3, Platform: PlatformOpenAI, Status: StatusError, Concurrency: 1},
	})

	for _, tc := range []struct {
		name      string
		accountID int64
	}{
		{name: "missing account", accountID: 999},
		{name: "non openai platform", accountID: 1},
		{name: "disabled account", accountID: 2},
		{name: "error account", accountID: 3},
		{name: "zero id", accountID: 0},
		{name: "negative id", accountID: -5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selection, err := svc.SelectPinnedAccount(context.Background(), tc.accountID)
			require.Nil(t, selection)
			require.Error(t, err)
			require.True(t, errors.Is(err, ErrPinnedAccountUnavailable))
		})
	}
}

func TestSelectPinnedAccountNilRepoIsUnavailable(t *testing.T) {
	svc := &OpenAIGatewayService{}
	selection, err := svc.SelectPinnedAccount(context.Background(), 1)
	require.Nil(t, selection)
	require.True(t, errors.Is(err, ErrPinnedAccountUnavailable))
}
