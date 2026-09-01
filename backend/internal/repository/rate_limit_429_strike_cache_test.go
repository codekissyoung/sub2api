package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestRateLimit429StrikeCacheAdaptiveLadderAndReset(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	cache := NewRateLimit429StrikeCache(rdb)
	ctx := context.Background()

	first, err := cache.RegisterRateLimit429Strike(ctx, 42, 10*time.Second, 30*time.Second, 60*time.Second, 2*time.Minute)
	require.NoError(t, err)
	require.Equal(t, int64(1), first.Strike)
	require.True(t, first.Advanced)
	require.WithinDuration(t, time.Now().Add(10*time.Second), first.ResetAt, 2*time.Second)

	duplicate, err := cache.RegisterRateLimit429Strike(ctx, 42, 10*time.Second, 30*time.Second, 60*time.Second, 2*time.Minute)
	require.NoError(t, err)
	require.Equal(t, int64(1), duplicate.Strike)
	require.False(t, duplicate.Advanced)
	require.Equal(t, first.ResetAt, duplicate.ResetAt)

	key := fmt.Sprintf("%s%d", rateLimit429StrikePrefix, 42)
	require.NoError(t, rdb.HSet(ctx, key, "blocked_until_ms", time.Now().Add(-time.Second).UnixMilli()).Err())
	second, err := cache.RegisterRateLimit429Strike(ctx, 42, 10*time.Second, 30*time.Second, 60*time.Second, 2*time.Minute)
	require.NoError(t, err)
	require.Equal(t, int64(2), second.Strike)
	require.True(t, second.Advanced)
	require.WithinDuration(t, time.Now().Add(30*time.Second), second.ResetAt, 2*time.Second)

	require.NoError(t, rdb.HSet(ctx, key, "blocked_until_ms", time.Now().Add(-time.Second).UnixMilli()).Err())
	third, err := cache.RegisterRateLimit429Strike(ctx, 42, 10*time.Second, 30*time.Second, 60*time.Second, 2*time.Minute)
	require.NoError(t, err)
	require.Equal(t, int64(3), third.Strike)
	require.True(t, third.Advanced)
	require.WithinDuration(t, time.Now().Add(60*time.Second), third.ResetAt, 2*time.Second)

	require.NoError(t, cache.ResetRateLimit429Strike(ctx, 42))
	afterSuccess, err := cache.RegisterRateLimit429Strike(ctx, 42, 10*time.Second, 30*time.Second, 60*time.Second, 2*time.Minute)
	require.NoError(t, err)
	require.Equal(t, int64(1), afterSuccess.Strike)
	require.True(t, afterSuccess.Advanced)
}

func TestRateLimit429StrikeCacheWindowExpiryStartsOver(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	cache := NewRateLimit429StrikeCache(rdb)
	ctx := context.Background()

	_, err := cache.RegisterRateLimit429Strike(ctx, 9, time.Second, 2*time.Second, 3*time.Second, 5*time.Second)
	require.NoError(t, err)
	key := fmt.Sprintf("%s%d", rateLimit429StrikePrefix, 9)
	require.NoError(t, rdb.HSet(ctx, key,
		"last_ms", time.Now().Add(-6*time.Second).UnixMilli(),
		"blocked_until_ms", time.Now().Add(-time.Second).UnixMilli(),
	).Err())

	decision, err := cache.RegisterRateLimit429Strike(ctx, 9, time.Second, 2*time.Second, 3*time.Second, 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, int64(1), decision.Strike)
	require.True(t, decision.Advanced)
}
