package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIPoolEstimate_NormalValues(t *testing.T) {
	accounts := []Account{
		{Extra: map[string]any{"codex_primary_used_percent": 25.0}}, // 剩余 75% → $1500
		{Extra: map[string]any{"codex_primary_used_percent": 75.0}}, // 剩余 25% → $500
		{Extra: map[string]any{"codex_primary_used_percent": 0.0}},  // 满血 → $2000
	}
	estimate := EstimateOpenAIPoolValue(accounts)
	require.InEpsilon(t, 4000.0, estimate.TotalUSD, 1e-9)
	require.Equal(t, 3, estimate.AccountCount)
	require.Equal(t, 0, estimate.MissingDataCount)
	require.Nil(t, estimate.NewestUsageUpdatedAt)
	require.Nil(t, estimate.OldestUsageUpdatedAt)
}

func TestOpenAIPoolEstimate_FallbackLegacyKey(t *testing.T) {
	accounts := []Account{
		{Extra: map[string]any{"codex_7d_used_percent": 50.0}}, // 旧键名 → 剩余 50% → $1000
	}
	estimate := EstimateOpenAIPoolValue(accounts)
	require.InEpsilon(t, 1000.0, estimate.TotalUSD, 1e-9)
	require.Equal(t, 1, estimate.AccountCount)
	require.Equal(t, 0, estimate.MissingDataCount)
}

func TestOpenAIPoolEstimate_PrimaryKeyPreferredOverLegacy(t *testing.T) {
	accounts := []Account{
		{Extra: map[string]any{
			"codex_primary_used_percent": 10.0,
			"codex_7d_used_percent":      90.0,
		}}, // 优先新键名 → 剩余 90% → $1800
	}
	estimate := EstimateOpenAIPoolValue(accounts)
	require.InEpsilon(t, 1800.0, estimate.TotalUSD, 1e-9)
	require.Equal(t, 1, estimate.AccountCount)
}

func TestOpenAIPoolEstimate_MissingOrUnparseableData(t *testing.T) {
	accounts := []Account{
		{Extra: nil}, // 无 Extra
		{Extra: map[string]any{}},
		{Extra: map[string]any{"codex_primary_used_percent": "not-a-number"}},
		{Extra: map[string]any{"codex_primary_used_percent": nil}},
		{Extra: map[string]any{"codex_primary_used_percent": 40.0}}, // 正常 → $1200
	}
	estimate := EstimateOpenAIPoolValue(accounts)
	require.InEpsilon(t, 1200.0, estimate.TotalUSD, 1e-9)
	require.Equal(t, 1, estimate.AccountCount)
	require.Equal(t, 4, estimate.MissingDataCount)
}

func TestOpenAIPoolEstimate_ClampOutOfRange(t *testing.T) {
	accounts := []Account{
		{Extra: map[string]any{"codex_primary_used_percent": 150.0}},  // 超限 → 剩余 0% → $0（仍计入号数）
		{Extra: map[string]any{"codex_primary_used_percent": -20.0}},  // 负值 → 剩余 100% → $2000
		{Extra: map[string]any{"codex_primary_used_percent": 100.0}},  // 用尽 → $0
		{Extra: map[string]any{"codex_primary_used_percent": "30.5"}}, // 字符串数字 → 剩余 69.5% → $1390
	}
	estimate := EstimateOpenAIPoolValue(accounts)
	require.InEpsilon(t, 3390.0, estimate.TotalUSD, 1e-9)
	require.Equal(t, 4, estimate.AccountCount)
	require.Equal(t, 0, estimate.MissingDataCount)
}

func TestOpenAIPoolEstimate_UsageUpdatedAtRange(t *testing.T) {
	accounts := []Account{
		{Extra: map[string]any{
			"codex_primary_used_percent": 10.0,
			"codex_usage_updated_at":     "2026-08-10T12:00:00Z",
		}},
		{Extra: map[string]any{
			"codex_primary_used_percent": 20.0,
			"codex_usage_updated_at":     "2026-08-13T08:30:00+08:00", // UTC 2026-08-13T00:30:00Z
		}},
		{Extra: map[string]any{
			"codex_primary_used_percent": 30.0,
			"codex_usage_updated_at":     "bad-timestamp", // 解析失败，忽略
		}},
		{Extra: map[string]any{}}, // 无时间戳
	}
	estimate := EstimateOpenAIPoolValue(accounts)
	require.NotNil(t, estimate.OldestUsageUpdatedAt)
	require.NotNil(t, estimate.NewestUsageUpdatedAt)
	require.Equal(t, "2026-08-10T12:00:00Z", *estimate.OldestUsageUpdatedAt)
	require.Equal(t, "2026-08-13T00:30:00Z", *estimate.NewestUsageUpdatedAt)
}

func TestOpenAIPoolEstimate_EmptyPool(t *testing.T) {
	estimate := EstimateOpenAIPoolValue(nil)
	require.Zero(t, estimate.TotalUSD)
	require.Zero(t, estimate.AccountCount)
	require.Zero(t, estimate.MissingDataCount)
	require.Nil(t, estimate.NewestUsageUpdatedAt)
	require.Nil(t, estimate.OldestUsageUpdatedAt)
}
