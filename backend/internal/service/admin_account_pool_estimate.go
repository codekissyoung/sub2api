package service

import (
	"fmt"
	"time"
)

// OpenAIPoolDollarsPerRemainingPercent 估值换算基准：周限剩余 1% ≈ $20（经验值）。
// 即满血（已用 0%）单号估值 $2000。
const OpenAIPoolDollarsPerRemainingPercent = 20.0

// OpenAIPoolEstimate 是 OpenAI OAuth 号池剩余金额的估值汇总。
type OpenAIPoolEstimate struct {
	TotalUSD             float64 `json:"total_usd"`
	AccountCount         int     `json:"account_count"`           // 参与计价的账号数
	MissingDataCount     int     `json:"missing_data_count"`      // 缺少/无法解析用量数据、未计入金额的账号数
	NewestUsageUpdatedAt *string `json:"newest_usage_updated_at"` // 最新一份用量数据的缓存时间（RFC3339），无数据时为 null
	OldestUsageUpdatedAt *string `json:"oldest_usage_updated_at"` // 最旧一份用量数据的缓存时间（RFC3339），无数据时为 null
}

// EstimateOpenAIPoolValue 根据账号 Extra 中缓存的周限用量数据估算号池剩余金额。
// 单号估值 = clamp(100-已用%, 0, 100) × OpenAIPoolDollarsPerRemainingPercent；
// 已用% 取自 codex_primary_used_percent（旧键名 fallback codex_7d_used_percent），
// 缺失或解析失败的账号不计入金额，计入 MissingDataCount。
func EstimateOpenAIPoolValue(accounts []Account) OpenAIPoolEstimate {
	estimate := OpenAIPoolEstimate{}
	var newest, oldest time.Time
	for i := range accounts {
		account := &accounts[i]

		if raw, ok := account.Extra["codex_usage_updated_at"]; ok && raw != nil {
			if ts, err := parseTime(fmt.Sprint(raw)); err == nil {
				if newest.IsZero() || ts.After(newest) {
					newest = ts
				}
				if oldest.IsZero() || ts.Before(oldest) {
					oldest = ts
				}
			}
		}

		usedPercent, ok := resolveAccountExtraNumber(account.Extra, "codex_primary_used_percent", "codex_7d_used_percent")
		if !ok {
			estimate.MissingDataCount++
			continue
		}
		remaining := 100 - usedPercent
		if remaining < 0 {
			remaining = 0
		}
		if remaining > 100 {
			remaining = 100
		}
		estimate.TotalUSD += remaining * OpenAIPoolDollarsPerRemainingPercent
		estimate.AccountCount++
	}
	if !newest.IsZero() {
		formatted := newest.UTC().Format(time.RFC3339)
		estimate.NewestUsageUpdatedAt = &formatted
	}
	if !oldest.IsZero() {
		formatted := oldest.UTC().Format(time.RFC3339)
		estimate.OldestUsageUpdatedAt = &formatted
	}
	return estimate
}
