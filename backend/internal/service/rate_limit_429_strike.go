package service

import (
	"context"
	"time"
)

// RateLimit429StrikeDecision is the distributed adaptive cooldown decision for
// a generic OpenAI 429 that did not include a provider reset time.
type RateLimit429StrikeDecision struct {
	Strike   int64
	ResetAt  time.Time
	Advanced bool
}

// RateLimit429StrikeCache coordinates generic OpenAI 429 strikes across all
// Sub2API instances sharing Redis. Register coalesces concurrent 429s that land
// inside the active cooldown so one burst advances the ladder only once.
type RateLimit429StrikeCache interface {
	RegisterRateLimit429Strike(
		ctx context.Context,
		accountID int64,
		firstCooldown time.Duration,
		secondCooldown time.Duration,
		thirdCooldown time.Duration,
		strikeWindow time.Duration,
	) (*RateLimit429StrikeDecision, error)
	ResetRateLimit429Strike(ctx context.Context, accountID int64) error
}
