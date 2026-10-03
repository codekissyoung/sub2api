//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 凭据证活否决（2026-10-04 事故修复）：accounts.expires_at 只在创建/编辑时写入，
// 从不随订阅续期回写；credentials.subscription_expires_at 随 OAuth 刷新持续更新。
// 凡按 expires_at 判定过期的路径（AutoPauseExpiredAccounts 与调度侧
// notExpiredPredicate），只要凭据证明订阅仍活就必须否决。
func TestAutoPauseExpiredAccountsCredentialVeto(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newAccountRepositoryWithSQL(client, tx, nil)

	now := time.Now()
	past := now.Add(-time.Hour)
	futureSub := now.Add(15 * 24 * time.Hour).UTC().Format(time.RFC3339)     // Z 形
	pastSub := now.Add(-time.Hour).UTC().Format("2006-01-02T15:04:05+00:00") // 偏移形（生产实测格式）
	creds := func(sub string) map[string]any {
		return map[string]any{"access_token": "redacted", "subscription_expires_at": sub}
	}

	alive := mustCreateAccount(t, client, &service.Account{
		Name: "autopause-cred-alive", Credentials: creds(futureSub),
	})
	expiredSub := mustCreateAccount(t, client, &service.Account{
		Name: "autopause-cred-expired", Credentials: creds(pastSub),
	})
	noCred := mustCreateAccount(t, client, &service.Account{
		Name: "autopause-cred-missing", Credentials: map[string]any{"access_token": "redacted"},
	})
	garbage := mustCreateAccount(t, client, &service.Account{
		Name: "autopause-cred-garbage", Credentials: creds("not-a-date"),
	})
	fresh := mustCreateAccount(t, client, &service.Account{
		Name: "autopause-not-yet-expired", Credentials: map[string]any{},
	})

	// 前四个：账号级 expires_at 已过期且 auto_pause 开启；fresh 未到期作对照。
	for _, a := range []*service.Account{alive, expiredSub, noCred, garbage} {
		_, err := client.Account.UpdateOneID(a.ID).
			SetExpiresAt(past).SetAutoPauseOnExpired(true).Save(ctx)
		require.NoError(t, err)
	}
	_, err := client.Account.UpdateOneID(fresh.ID).
		SetExpiresAt(now.Add(time.Hour)).SetAutoPauseOnExpired(true).Save(ctx)
	require.NoError(t, err)

	updated, err := repo.AutoPauseExpiredAccounts(ctx, now)
	require.NoError(t, err)
	require.EqualValues(t, 3, updated, "凭据活着的号必须被否决；凭据过期/缺失/脏值仍按字段暂停")

	assertSchedulable := func(id int64, want bool) {
		row, err := client.Account.Get(ctx, id)
		require.NoError(t, err)
		require.Equal(t, want, row.Schedulable, "account %d", id)
	}
	assertSchedulable(alive.ID, true)
	assertSchedulable(expiredSub.ID, false)
	assertSchedulable(noCred.ID, false)
	assertSchedulable(garbage.ID, false)
	assertSchedulable(fresh.ID, true)

	// 调度侧读取路径同语义：凭据证活的号必须仍可被调度。
	listed, err := repo.ListSchedulable(ctx)
	require.NoError(t, err)
	ids := make(map[int64]bool, len(listed))
	for _, a := range listed {
		ids[a.ID] = true
	}
	require.True(t, ids[alive.ID], "凭据活着的过期字段号必须仍在调度集")
	require.True(t, ids[fresh.ID])
	require.False(t, ids[expiredSub.ID])
	require.False(t, ids[noCred.ID])
	require.False(t, ids[garbage.ID])
}
