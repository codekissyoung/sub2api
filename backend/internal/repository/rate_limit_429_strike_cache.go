package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const rateLimit429StrikePrefix = "rate_limit_429_strike:account:"

// registerRateLimit429StrikeScript advances one cooldown generation at a time.
// Concurrent/in-flight 429s received before blocked_until_ms reuse the current
// generation and reset timestamp instead of immediately escalating the ladder.
var registerRateLimit429StrikeScript = redis.NewScript(`
	local key = KEYS[1]
	local first_ms = tonumber(ARGV[1])
	local second_ms = tonumber(ARGV[2])
	local third_ms = tonumber(ARGV[3])
	local window_ms = tonumber(ARGV[4])

	local redis_time = redis.call('TIME')
	local now_ms = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)
	local count = tonumber(redis.call('HGET', key, 'count')) or 0
	local last_ms = tonumber(redis.call('HGET', key, 'last_ms')) or 0
	local blocked_until_ms = tonumber(redis.call('HGET', key, 'blocked_until_ms')) or 0

	if blocked_until_ms > now_ms then
		redis.call('EXPIRE', key, math.ceil(math.max(window_ms, third_ms) / 1000))
		return {count, blocked_until_ms, 0}
	end

	if last_ms == 0 or now_ms - last_ms > window_ms then
		count = 0
	end
	count = count + 1

	local cooldown_ms = third_ms
	if count == 1 then
		cooldown_ms = first_ms
	elseif count == 2 then
		cooldown_ms = second_ms
	end
	blocked_until_ms = now_ms + cooldown_ms

	redis.call('HSET', key,
		'count', count,
		'last_ms', now_ms,
		'blocked_until_ms', blocked_until_ms)
	redis.call('EXPIRE', key, math.ceil(math.max(window_ms, third_ms) / 1000))
	return {count, blocked_until_ms, 1}
`)

type rateLimit429StrikeCache struct {
	rdb *redis.Client
}

func NewRateLimit429StrikeCache(rdb *redis.Client) service.RateLimit429StrikeCache {
	return &rateLimit429StrikeCache{rdb: rdb}
}

func (c *rateLimit429StrikeCache) RegisterRateLimit429Strike(
	ctx context.Context,
	accountID int64,
	firstCooldown time.Duration,
	secondCooldown time.Duration,
	thirdCooldown time.Duration,
	strikeWindow time.Duration,
) (*service.RateLimit429StrikeDecision, error) {
	if c == nil || c.rdb == nil {
		return nil, fmt.Errorf("rate limit 429 strike cache is not configured")
	}
	if accountID <= 0 {
		return nil, fmt.Errorf("account id must be positive")
	}
	if firstCooldown <= 0 || secondCooldown < firstCooldown || thirdCooldown < secondCooldown {
		return nil, fmt.Errorf("adaptive cooldowns must be positive and non-decreasing")
	}
	if strikeWindow < thirdCooldown {
		return nil, fmt.Errorf("strike window must cover the maximum cooldown")
	}

	key := fmt.Sprintf("%s%d", rateLimit429StrikePrefix, accountID)
	result, err := registerRateLimit429StrikeScript.Run(
		ctx,
		c.rdb,
		[]string{key},
		firstCooldown.Milliseconds(),
		secondCooldown.Milliseconds(),
		thirdCooldown.Milliseconds(),
		strikeWindow.Milliseconds(),
	).Slice()
	if err != nil {
		return nil, fmt.Errorf("register rate limit 429 strike: %w", err)
	}
	if len(result) != 3 {
		return nil, fmt.Errorf("register rate limit 429 strike: unexpected result length %d", len(result))
	}
	strike, err := redisResultInt64(result[0])
	if err != nil {
		return nil, fmt.Errorf("register rate limit 429 strike count: %w", err)
	}
	resetAtMillis, err := redisResultInt64(result[1])
	if err != nil {
		return nil, fmt.Errorf("register rate limit 429 strike reset: %w", err)
	}
	advancedRaw, err := redisResultInt64(result[2])
	if err != nil {
		return nil, fmt.Errorf("register rate limit 429 strike advanced: %w", err)
	}

	return &service.RateLimit429StrikeDecision{
		Strike:   strike,
		ResetAt:  time.UnixMilli(resetAtMillis),
		Advanced: advancedRaw == 1,
	}, nil
}

func (c *rateLimit429StrikeCache) ResetRateLimit429Strike(ctx context.Context, accountID int64) error {
	if c == nil || c.rdb == nil || accountID <= 0 {
		return nil
	}
	key := fmt.Sprintf("%s%d", rateLimit429StrikePrefix, accountID)
	return c.rdb.Del(ctx, key).Err()
}

func redisResultInt64(value any) (int64, error) {
	switch typed := value.(type) {
	case int64:
		return typed, nil
	case string:
		var parsed int64
		if _, err := fmt.Sscan(typed, &parsed); err != nil {
			return 0, err
		}
		return parsed, nil
	case []byte:
		var parsed int64
		if _, err := fmt.Sscan(string(typed), &parsed); err != nil {
			return 0, err
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("unexpected Redis integer type %T", value)
	}
}
