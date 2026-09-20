package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func fakeCodexTicketState(n int) string {
	if n < len(openAICodexTicketStatePrefix) {
		return strings.Repeat("A", n)
	}
	return openAICodexTicketStatePrefix + strings.Repeat("B", n-len(openAICodexTicketStatePrefix))
}

func ticketTestAccount(id int64) *Account {
	return &Account{
		ID:          id,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "tok", "chatgpt_account_id": "acc-1"},
	}
}

func ticketTestService(t *testing.T, cfg config.OpenAICodexTicketConfig, upstream HTTPUpstream) *OpenAIGatewayService {
	t.Helper()
	return &OpenAIGatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{OpenAICodexTicket: cfg},
		},
		httpUpstream: upstream,
	}
}

// ticketEnforceSettings 返回注入演练关闭（dry_run=false）的运行时设置，
// 让 applyOpenAICodexTicket 真正改写请求头、fail_closed 门控真实生效。
func ticketEnforceSettings() *SettingService {
	repo := &codexTicketSettingRepo{codexPolicyMigrationRepoStub: &codexPolicyMigrationRepoStub{values: map[string]string{
		SettingKeyOpenAICodexTicketInjectDryRun: "false",
	}}}
	return NewSettingService(repo, &config.Config{})
}

func ticketTestServiceEnforce(t *testing.T, cfg config.OpenAICodexTicketConfig, upstream HTTPUpstream) *OpenAIGatewayService {
	t.Helper()
	svc := ticketTestService(t, cfg, upstream)
	svc.settingService = ticketEnforceSettings()
	return svc
}

func TestDecideOpenAICodexTicketAction(t *testing.T) {
	cfg := config.OpenAICodexTicketConfig{NormalLength: 292, DegradedLength: 312}
	ticket := &openAICodexTicket{State: fakeCodexTicketState(292), Length: 292, ExpiresAt: time.Now().Add(time.Hour)}
	require.Equal(t, openAICodexTicketActionPassNoTicket, decideOpenAICodexTicketAction(nil, "", cfg))
	require.Equal(t, openAICodexTicketActionPassNoTicket, decideOpenAICodexTicketAction(nil, fakeCodexTicketState(312), cfg))
	require.Equal(t, openAICodexTicketActionInject, decideOpenAICodexTicketAction(ticket, "", cfg))
	require.Equal(t, openAICodexTicketActionInject, decideOpenAICodexTicketAction(ticket, "  ", cfg))
	require.Equal(t, openAICodexTicketActionKeepClient, decideOpenAICodexTicketAction(ticket, fakeCodexTicketState(292), cfg))
	require.Equal(t, openAICodexTicketActionReplace, decideOpenAICodexTicketAction(ticket, fakeCodexTicketState(312), cfg))
	require.Equal(t, openAICodexTicketActionKeepUnknown, decideOpenAICodexTicketAction(ticket, fakeCodexTicketState(300), cfg))
	require.Equal(t, openAICodexTicketActionKeepUnknown, decideOpenAICodexTicketAction(ticket, "stale", cfg))
}

func TestOpenAICodexTicketShapeClassification(t *testing.T) {
	cfg := config.OpenAICodexTicketConfig{}
	require.Equal(t, openAICodexTicketShapeNormal, openAICodexTicketShapeForLength(292, cfg))
	require.Equal(t, openAICodexTicketShapeDegraded, openAICodexTicketShapeForLength(312, cfg))
	require.Equal(t, openAICodexTicketShapeUnknown, openAICodexTicketShapeForLength(300, cfg))
	require.Equal(t, openAICodexTicketShapeUnknown, openAICodexTicketShapeForLength(0, cfg))
	// 自定义长度（如 Team 账号 332/356）走配置。
	team := config.OpenAICodexTicketConfig{NormalLength: 332, DegradedLength: 356}
	require.Equal(t, openAICodexTicketShapeNormal, openAICodexTicketShapeForLength(332, team))
	require.Equal(t, openAICodexTicketShapeDegraded, openAICodexTicketShapeForLength(356, team))
	require.Equal(t, openAICodexTicketShapeUnknown, openAICodexTicketShapeForLength(292, team))
}

// 同桶两种形状共存：292 进正常槽（可注入），312 进降级槽（仅标记），互不顶掉。
func TestCaptureOpenAICodexTicket_KeepsNormalAndDegradedSlots(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, TTLSeconds: 3600}, nil)
	account := ticketTestAccount(41)

	normalState := fakeCodexTicketState(292)
	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, normalState)
	svc.captureOpenAICodexTicket(account, "gpt-6-astra", h)

	degradedState := fakeCodexTicketState(312)
	h = http.Header{}
	h.Set(openAICodexTurnStateHeader, degradedState)
	svc.captureOpenAICodexTicket(account, "gpt-6-astra", h)

	bucket := svc.lookupOpenAICodexTicketBucket(account, "gpt-6-astra")
	require.NotNil(t, bucket)
	require.NotNil(t, bucket.Normal)
	require.Equal(t, normalState, bucket.Normal.State)
	require.NotNil(t, bucket.Degraded)
	require.Equal(t, degradedState, bucket.Degraded.State)
	// 降级捕获不影响正常票的可注入性。
	require.NotNil(t, svc.injectableOpenAICodexTicket(account, "gpt-6-astra", time.Now(), svc.openAICodexTicketConfig()))
}

// 演练模式（默认）：跑决策树但不改写请求头、fail_closed 不拦、调度不 block。
func TestApplyOpenAICodexTicket_DryRunObservesOnly(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:    true,
		Inject:     true,
		TTLSeconds: 3600,
		FailClosed: true,
	}, nil)
	account := ticketTestAccount(41)
	svc.storeOpenAICodexTicket(context.Background(), account, &openAICodexTicket{
		AccountID:  41,
		Model:      "gpt-6-astra",
		State:      fakeCodexTicketState(292),
		Length:     292,
		CapturedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	})

	// 客户端带回降级态，决策为 replace，但演练不改写。
	h := http.Header{}
	degraded := fakeCodexTicketState(312)
	h.Set(openAICodexTurnStateHeader, degraded)
	require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), account, "gpt-6-astra", h))
	require.Equal(t, degraded, h.Get(openAICodexTurnStateHeader))

	// 无票账号：fail_closed 在演练下不报错、不拦调度。
	noTicket := ticketTestAccount(42)
	h = http.Header{}
	require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), noTicket, "gpt-6-astra", h))
	require.Empty(t, h.Get(openAICodexTurnStateHeader))
	require.False(t, svc.openAICodexTicketBlocksAccount(noTicket, "gpt-6-astra"))
}

// 正式模式决策树：客户端带回正常态不覆盖；带回降级态替换；未带补票；未知形状不动。
func TestApplyOpenAICodexTicket_EnforceDecisionTree(t *testing.T) {
	svc := ticketTestServiceEnforce(t, config.OpenAICodexTicketConfig{
		Enabled:    true,
		Inject:     true,
		TTLSeconds: 3600,
	}, nil)
	account := ticketTestAccount(41)
	normalState := fakeCodexTicketState(292)
	svc.storeOpenAICodexTicket(context.Background(), account, &openAICodexTicket{
		AccountID:  41,
		Model:      "gpt-6-astra",
		State:      normalState,
		Length:     292,
		CapturedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	})

	// 客户端带回正常态 → 不动（它的更新鲜）。
	clientNormal := openAICodexTicketStatePrefix + strings.Repeat("C", 286)
	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, clientNormal)
	require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), account, "gpt-6-astra", h))
	require.Equal(t, clientNormal, h.Get(openAICodexTurnStateHeader))

	// 客户端带回降级态 → 替换为库里的正常票。
	h = http.Header{}
	h.Set(openAICodexTurnStateHeader, fakeCodexTicketState(312))
	require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), account, "gpt-6-astra", h))
	require.Equal(t, normalState, h.Get(openAICodexTurnStateHeader))

	// 客户端未带 → 补票。
	h = http.Header{}
	require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), account, "gpt-6-astra", h))
	require.Equal(t, normalState, h.Get(openAICodexTurnStateHeader))

	// 客户端带回未知形状 → 保守不动。
	h = http.Header{}
	h.Set(openAICodexTurnStateHeader, fakeCodexTicketState(300))
	require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), account, "gpt-6-astra", h))
	require.Equal(t, fakeCodexTicketState(300), h.Get(openAICodexTurnStateHeader))
}

// 注入安全边际：正常票剩余有效期不足 inject_min_remaining_seconds 时不注入。
func TestApplyOpenAICodexTicket_ExpiringSoonNotInjected(t *testing.T) {
	svc := ticketTestServiceEnforce(t, config.OpenAICodexTicketConfig{
		Enabled:    true,
		Inject:     true,
		TTLSeconds: 3600,
		FailClosed: true,
	}, nil)
	account := ticketTestAccount(41)
	svc.storeOpenAICodexTicket(context.Background(), account, &openAICodexTicket{
		AccountID:  41,
		Model:      "gpt-6-astra",
		State:      fakeCodexTicketState(292),
		Length:     292,
		CapturedAt: time.Now().Add(-55 * time.Minute),
		ExpiresAt:  time.Now().Add(5 * time.Minute), // 只剩 5 分钟 < 600s 边际
	})
	h := http.Header{}
	err := svc.applyOpenAICodexTicket(context.Background(), account, "gpt-6-astra", h)
	require.ErrorIs(t, err, ErrOpenAICodexTicketUnavailable)
	require.Empty(t, h.Get(openAICodexTurnStateHeader))
}

func TestApplyOpenAICodexTicket_ReplacesHeader(t *testing.T) {
	state := fakeCodexTicketState(292)
	svc := ticketTestServiceEnforce(t, config.OpenAICodexTicketConfig{
		Enabled:    true,
		Inject:     true,
		TTLSeconds: 3600,
		FailClosed: true,
	}, nil)
	account := ticketTestAccount(41)
	svc.storeOpenAICodexTicket(context.Background(), account, &openAICodexTicket{
		AccountID:  41,
		Model:      "gpt-6-astra",
		State:      state,
		Length:     292,
		CapturedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	})

	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, fakeCodexTicketState(312))
	err := svc.applyOpenAICodexTicket(context.Background(), account, "gpt-6-astra", h)
	require.NoError(t, err)
	require.Equal(t, state, h.Get(openAICodexTurnStateHeader))
	require.Equal(t, 292, len(h.Get(openAICodexTurnStateHeader)))
}

func TestApplyOpenAICodexTicket_DoesNotReuseOtherModelOrAccount(t *testing.T) {
	svc := ticketTestServiceEnforce(t, config.OpenAICodexTicketConfig{
		Enabled:         true,
		Inject:          true,
		TTLSeconds:      3600,
		FailClosed:      true,
		HarvestProxyURL: "socks5h://harvest",
	}, &httpUpstreamRecorder{err: io.EOF})
	a := ticketTestAccount(41)
	b := ticketTestAccount(42)
	astra := fakeCodexTicketState(292)
	svc.storeOpenAICodexTicket(context.Background(), a, &openAICodexTicket{
		AccountID:  41,
		Model:      "gpt-6-astra",
		State:      astra,
		Length:     292,
		CapturedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	})

	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, "keep-ungated")
	err := svc.applyOpenAICodexTicket(context.Background(), a, "gpt-5.5", h)
	require.NoError(t, err)
	require.Equal(t, "keep-ungated", h.Get(openAICodexTurnStateHeader))
	require.False(t, svc.openAICodexTicketBlocksAccount(a, "gpt-5.5"))
	require.True(t, svc.openAICodexTicketBlocksAccount(b, "gpt-6-astra"))
	require.False(t, svc.openAICodexTicketBlocksAccount(a, "gpt-6-astra"))

	h = http.Header{}
	err = svc.applyOpenAICodexTicket(context.Background(), b, "gpt-6-astra", h)
	require.ErrorIs(t, err, ErrOpenAICodexTicketUnavailable)
	require.Empty(t, h.Get(openAICodexTurnStateHeader))
}

func TestLookupOpenAICodexTicket_PrefersNewerExtra(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, TTLSeconds: 3600}, nil)
	account := ticketTestAccount(41)
	oldState := fakeCodexTicketState(292)
	newState := openAICodexTicketStatePrefix + strings.Repeat("C", 286)
	svc.storeOpenAICodexTicket(context.Background(), account, &openAICodexTicket{
		AccountID:  41,
		Model:      "gpt-6-astra",
		State:      oldState,
		Length:     292,
		CapturedAt: time.Now().Add(-30 * time.Minute),
		ExpiresAt:  time.Now().Add(-time.Minute),
	})
	account.Extra = map[string]any{openAICodexTicketExtraKey("gpt-6-astra"): &openAICodexTicket{
		Model:      "gpt-6-astra",
		State:      newState,
		Length:     292,
		CapturedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	},
	}
	bucket := svc.lookupOpenAICodexTicketBucket(account, "gpt-6-astra")
	require.NotNil(t, bucket)
	require.NotNil(t, bucket.Normal)
	require.Equal(t, newState, bucket.Normal.State)
	require.True(t, bucket.Normal.valid(time.Now(), 0))
}

func TestApplyOpenAICodexTicket_ExpiredNotInjected(t *testing.T) {
	svc := ticketTestServiceEnforce(t, config.OpenAICodexTicketConfig{
		Enabled:         true,
		Inject:          true,
		TTLSeconds:      3600,
		FailClosed:      true,
		HarvestProxyURL: "socks5h://harvest",
	}, &httpUpstreamRecorder{err: io.EOF})
	account := ticketTestAccount(41)
	svc.storeOpenAICodexTicket(context.Background(), account, &openAICodexTicket{
		AccountID:  41,
		Model:      "gpt-6-astra",
		State:      fakeCodexTicketState(292),
		Length:     292,
		CapturedAt: time.Now().Add(-2 * time.Hour),
		ExpiresAt:  time.Now().Add(-time.Minute),
	})
	h := http.Header{}
	err := svc.applyOpenAICodexTicket(context.Background(), account, "gpt-6-astra", h)
	require.ErrorIs(t, err, ErrOpenAICodexTicketUnavailable)
	require.Empty(t, h.Get(openAICodexTurnStateHeader))
}

// 降级形状（312）只进降级槽作标记，不进注入候选；fail_closed 下视为无票。
func TestApplyOpenAICodexTicket_WrongLengthNotInjected(t *testing.T) {
	svc := ticketTestServiceEnforce(t, config.OpenAICodexTicketConfig{
		Enabled:    true,
		Inject:     true,
		TTLSeconds: 3600,
		FailClosed: true,
	}, nil)
	account := ticketTestAccount(41)
	svc.storeOpenAICodexTicket(context.Background(), account, &openAICodexTicket{
		AccountID:  41,
		Model:      "gpt-6-astra",
		State:      fakeCodexTicketState(312),
		Length:     312,
		CapturedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	})
	h := http.Header{}
	err := svc.applyOpenAICodexTicket(context.Background(), account, "gpt-6-astra", h)
	require.ErrorIs(t, err, ErrOpenAICodexTicketUnavailable)
	require.Empty(t, h.Get(openAICodexTurnStateHeader))
}

func TestApplyOpenAICodexTicket_FailOpenSkipsInject(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:    true,
		Inject:     true,
		FailClosed: false,
	}, nil)
	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, "client-state")
	err := svc.applyOpenAICodexTicket(context.Background(), ticketTestAccount(41), "gpt-6-astra", h)
	require.NoError(t, err)
	require.Equal(t, "client-state", h.Get(openAICodexTurnStateHeader))
	require.False(t, svc.openAICodexTicketBlocksAccount(ticketTestAccount(41), "gpt-6-astra"))
}

func TestApplyOpenAICodexTicket_DisabledNoop(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: false, FailClosed: true}, nil)
	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, "client-state")
	err := svc.applyOpenAICodexTicket(context.Background(), ticketTestAccount(41), "gpt-6-astra", h)
	require.NoError(t, err)
	require.Equal(t, "client-state", h.Get(openAICodexTurnStateHeader))
}

// 观察模式（enabled=true, inject=false）：打票照常，但业务出站不注入、缺票不拦调度。
func TestApplyOpenAICodexTicket_InjectDisabledObservesOnly(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:      true,
		Inject:       false,
		TargetLength: 292,
		TTLSeconds:   3600,
		FailClosed:   true,
	}, nil)
	account := ticketTestAccount(41)
	svc.storeOpenAICodexTicket(context.Background(), account, &openAICodexTicket{
		AccountID:  41,
		Model:      "gpt-6-astra",
		State:      fakeCodexTicketState(292),
		Length:     292,
		CapturedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	})

	// (a) 有票也不注入 header、不报错。
	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, "client-state")
	require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), account, "gpt-6-astra", h))
	require.Equal(t, "client-state", h.Get(openAICodexTurnStateHeader))

	// (b) 无票 + fail_closed=true 也不报错、不 block 调度。
	noTicket := ticketTestAccount(42)
	h = http.Header{}
	require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), noTicket, "gpt-6-astra", h))
	require.Empty(t, h.Get(openAICodexTurnStateHeader))
	require.False(t, svc.openAICodexTicketBlocksAccount(noTicket, "gpt-6-astra"))

	// (c) 管理端状态不计 Blocked。
	statuses := OpenAICodexTicketStatuses(noTicket, svc.openAICodexTicketConfig(), time.Now())
	require.NotEmpty(t, statuses)
	for _, status := range statuses {
		require.False(t, status.Blocked)
	}
}

func TestHarvestOpenAICodexTicket_StopsAt292AndUsesHarvestProxy(t *testing.T) {
	state312 := fakeCodexTicketState(312)
	state292 := fakeCodexTicketState(292)
	header312 := http.Header{}
	header312.Set(openAICodexTurnStateHeader, state312)
	header292 := http.Header{}
	header292.Set(openAICodexTurnStateHeader, state292)
	upstream := &httpUpstreamRecorder{
		responses: []*http.Response{
			{
				StatusCode: http.StatusOK,
				Header:     header312,
				Body:       io.NopCloser(strings.NewReader("data: {}\n\n")),
			},
			{
				StatusCode: http.StatusOK,
				Header:     header292,
				Body:       io.NopCloser(strings.NewReader("data: {}\n\n")),
			},
		},
	}
	svc := ticketTestServiceEnforce(t, config.OpenAICodexTicketConfig{
		Enabled:                      true,
		Inject:                       true,
		TargetLength:                 292,
		TTLSeconds:                   3600,
		HarvestProxyURL:              "socks5h://user:pass@harvest.example:31",
		HarvestAttemptTimeoutSeconds: 5,
		FailClosed:                   true,
	}, upstream)
	account := ticketTestAccount(41)

	svc.probeOnceOpenAICodexTicket(context.Background(), account, "gpt-6-astra")
	require.Nil(t, svc.lookupOpenAICodexTicketBucket(account, "gpt-6-astra"))
	svc.probeOnceOpenAICodexTicket(context.Background(), account, "gpt-6-astra")
	bucket := svc.lookupOpenAICodexTicketBucket(account, "gpt-6-astra")
	require.NotNil(t, bucket)
	require.NotNil(t, bucket.Normal)
	require.Equal(t, state292, bucket.Normal.State)
	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, fakeCodexTicketState(312))
	require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), account, "gpt-6-astra", h))
	require.Equal(t, state292, h.Get(openAICodexTurnStateHeader))
	require.Equal(t, "socks5h://user:pass@harvest.example:31", upstream.lastProxyURL)
	require.Len(t, upstream.requests, 2)
	require.Empty(t, upstream.requests[0].Header.Get(openAICodexTurnStateHeader))
	require.Equal(t, openAICodexAstraMinVersion, upstream.requests[0].Header.Get("version"))
	require.Equal(t, HTTPUpstreamProfileOpenAIHarvest, HTTPUpstreamProfileFromContext(upstream.requests[0].Context()))
	require.True(t, upstream.requests[0].Close)
}

func TestHarvestOpenAICodexTicket_HTTP503DoesNotAbortHunt(t *testing.T) {
	state292 := fakeCodexTicketState(292)
	header503 := http.Header{}
	header292 := http.Header{}
	header292.Set(openAICodexTurnStateHeader, state292)
	responses := make([]*http.Response, 0, 3)
	responses = append(responses, &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     header503,
		Body:       io.NopCloser(strings.NewReader(`{"error":"overloaded"}`)),
	})
	responses = append(responses, &http.Response{
		StatusCode: http.StatusOK,
		Header:     header292,
		Body:       io.NopCloser(strings.NewReader("data: {}\n\n")),
	})
	upstream := &httpUpstreamRecorder{responses: responses}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:                      true,
		TargetLength:                 292,
		TTLSeconds:                   3600,
		HarvestProxyURL:              "socks5h://harvest.example:31",
		HarvestAttemptTimeoutSeconds: 5,
		FailClosed:                   true,
	}, upstream)
	account := ticketTestAccount(41)
	svc.probeOnceOpenAICodexTicket(context.Background(), account, "gpt-6-astra")
	require.Nil(t, svc.lookupOpenAICodexTicketBucket(account, "gpt-6-astra"))
	svc.probeOnceOpenAICodexTicket(context.Background(), account, "gpt-6-astra")
	bucket := svc.lookupOpenAICodexTicketBucket(account, "gpt-6-astra")
	require.NotNil(t, bucket)
	require.NotNil(t, bucket.Normal)
	require.Equal(t, state292, bucket.Normal.State)
	require.Len(t, upstream.requests, 2)
}

func TestLookupOpenAICodexTicket_HydratesFromExtra(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, TTLSeconds: 3600}, nil)
	state := fakeCodexTicketState(292)
	account := ticketTestAccount(9)
	account.Extra = map[string]any{
		openAICodexTicketExtraKey("gpt-6-astra"): map[string]any{
			"state":       state,
			"length":      292,
			"model":       "gpt-6-astra",
			"captured_at": time.Now().Add(-time.Minute),
			"expires_at":  time.Now().Add(time.Hour),
		},
	}
	bucket := svc.lookupOpenAICodexTicketBucket(account, "gpt-6-astra")
	require.NotNil(t, bucket)
	require.NotNil(t, bucket.Normal)
	require.Equal(t, state, bucket.Normal.State)
	require.True(t, bucket.Normal.valid(time.Now(), 0))
}

func TestOpenAICodexTicketStatuses_ReportsRemainingTTL(t *testing.T) {
	account := ticketTestAccount(41)
	account.Extra = map[string]any{
		openAICodexTicketExtraKey("gpt-6-astra"): map[string]any{
			"state":       fakeCodexTicketState(292),
			"length":      292,
			"model":       "gpt-6-astra",
			"captured_at": time.Now().Add(-10 * time.Minute),
			"expires_at":  time.Now().Add(50 * time.Minute),
		},
	}
	now := time.Now()
	got := OpenAICodexTicketStatuses(account, config.OpenAICodexTicketConfig{Enabled: true, FailClosed: true}, now)
	require.Len(t, got, 2)
	require.Equal(t, "gpt-6-astra", got[0].Model)
	require.True(t, got[0].Ready)
	require.Equal(t, openAICodexTicketShapeNormal, got[0].Shape)
	require.False(t, got[0].Degraded)
	require.Greater(t, got[0].RemainingSeconds, int64(40*60))
	require.LessOrEqual(t, got[0].RemainingSeconds, int64(50*60))
	require.Equal(t, "gpt-5.6-sol", got[1].Model)
	require.False(t, got[1].Ready)
}

func TestExtractOpenAICodexTicketModel(t *testing.T) {
	require.Equal(t, "gpt-6-astra", extractOpenAICodexTicketModel([]byte(`{"model":"gpt-6-astra"}`)))
	require.Empty(t, extractOpenAICodexTicketModel([]byte(`{}`)))
}

// These stubs exercise the real continuous refresh path with both default models
// completing together. Run under -race to catch writes to the shared account maps.
type codexTicketRefreshRepo struct {
	AccountRepository
	accounts []Account
	mu       sync.Mutex
	updates  map[string]any
}

func (r *codexTicketRefreshRepo) ListByPlatform(context.Context, string) ([]Account, error) {
	return r.accounts, nil
}
func (r *codexTicketRefreshRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.updates == nil {
		r.updates = make(map[string]any)
	}
	for k, v := range updates {
		r.updates[k] = v
	}
	return nil
}

type codexTicketConcurrentUpstream struct {
	HTTPUpstream
	started atomic.Int64
	ready   chan struct{}
}

func (u *codexTicketConcurrentUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	if u.started.Add(1) == 2 {
		close(u.ready)
	}
	select {
	case <-u.ready:
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, fakeCodexTicketState(292))
	return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader("data: {}\n\n"))}, nil
}
func TestRefreshOpenAICodexTickets_ConcurrentModelsPreserveAccountSnapshot(t *testing.T) {
	account := ticketTestAccount(41)
	account.Status = StatusActive
	account.Extra = map[string]any{"existing": true}
	repo := &codexTicketRefreshRepo{accounts: []Account{*account}}
	upstream := &codexTicketConcurrentUpstream{ready: make(chan struct{})}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, Inject: true, HarvestProxyURL: "socks5h://proxy.example.com:1080"}, upstream)
	svc.settingService = ticketEnforceSettings()
	svc.accountRepo = repo
	svc.refreshOpenAICodexTickets(context.Background())
	require.Equal(t, int64(2), upstream.started.Load())
	require.Equal(t, map[string]any{"existing": true}, account.Extra)
	require.Len(t, repo.updates, 2)
	for _, model := range []string{openAICodexTicketDefaultModel, openAICodexTicketDefaultSolModel} {
		bucket := svc.lookupOpenAICodexTicketBucket(account, model)
		require.NotNil(t, bucket)
		require.NotNil(t, bucket.Normal)
		require.True(t, bucket.Normal.valid(time.Now(), 0))
	}
	// Valid tickets do not produce another probe on the next cycle.
	svc.refreshOpenAICodexTickets(context.Background())
	require.Equal(t, int64(2), upstream.started.Load())
}
func TestOpenAICodexTicketStatuses_RespectRuntimeConfiguration(t *testing.T) {
	account := ticketTestAccount(41)
	require.Empty(t, OpenAICodexTicketStatuses(account, config.OpenAICodexTicketConfig{}, time.Now()))
	cfg := config.OpenAICodexTicketConfig{Enabled: true, Inject: true, Models: []string{"custom-model"}}
	status := OpenAICodexTicketStatuses(account, cfg, time.Now())
	require.Len(t, status, 1)
	require.Equal(t, "custom-model", status[0].Model)
	require.False(t, status[0].Blocked)
	cfg.FailClosed = true
	require.True(t, OpenAICodexTicketStatuses(account, cfg, time.Now())[0].Blocked)
	cfg.Inject = false
	require.False(t, OpenAICodexTicketStatuses(account, cfg, time.Now())[0].Blocked)
}
func TestProbeOpenAICodexTicket_RejectsInvalidState(t *testing.T) {
	// target_length=292 严格模式：长度漂移（312）、缺 gAAAAA 前缀、空值都拒收。
	for _, state := range []string{fakeCodexTicketState(312), strings.Repeat("X", 292), ""} {
		h := http.Header{}
		h.Set(openAICodexTurnStateHeader, state)
		upstream := &httpUpstreamRecorder{responses: []*http.Response{{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(""))}}}
		svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, TargetLength: 292, HarvestProxyURL: "http://proxy.example.com:8080"}, upstream)
		account := ticketTestAccount(41)
		svc.probeOnceOpenAICodexTicket(context.Background(), account, "gpt-6-astra")
		require.Nil(t, svc.lookupOpenAICodexTicketBucket(account, "gpt-6-astra"))
	}
}
func TestOpenAICodexTicket_RequiresActualLengthAndExpiry(t *testing.T) {
	ticket := &openAICodexTicket{State: fakeCodexTicketState(312), Length: 292, ExpiresAt: time.Now().Add(time.Hour)}
	require.False(t, ticket.valid(time.Now(), 292))
	ticket.State = fakeCodexTicketState(292)
	ticket.ExpiresAt = time.Time{}
	require.False(t, ticket.valid(time.Now(), 292))
}

// /responses/compact 的出站模型被 Forward 改写为 gateway.openai_compact_model
// （默认非空），门票门控必须按该出站模型判定。否则对门控模型发 compact 请求时，
// 所有无票账号都会被 fail_closed 误判为不可调度，而这些请求实际不需要票。
func TestOpenAICodexTicketGate_CompactRequestUsesForwardOutboundModel(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
		OpenAICompactModel: "gpt-5.5",
		OpenAICodexTicket: config.OpenAICodexTicketConfig{
			Enabled:    true,
			Inject:     true,
			TTLSeconds: 3600,
			FailClosed: true,
			Models:     []string{"gpt-6-astra"},
		},
	}}}
	svc.settingService = ticketEnforceSettings()
	account := ticketTestAccount(41) // 无票

	// 出站模型预测必须与 Forward 的解析链一致。
	require.Equal(t, "gpt-6-astra", svc.openAICodexTicketOutboundModel(account, "gpt-6-astra", false))
	require.Equal(t, "gpt-5.5", svc.openAICodexTicketOutboundModel(account, "gpt-6-astra", true))

	// 普通请求：出站仍是门控模型且无票 → fail_closed 必须拦号。
	require.True(t, svc.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-6-astra", false))

	// compact 请求：出站已被改写成非门控的 gpt-5.5 → 不得拦号。
	require.False(t, svc.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-6-astra", true))

	// 回归锚点：按客户端原始模型判定（旧实现的口径）在 compact 下必然误拦。
	require.True(t, svc.openAICodexTicketBlocksAccount(account, canonicalOpenAIAccountSchedulingModel(account, "gpt-6-astra")))
}

// 被动捕获：真实业务响应里的 blob 直接落存，无长度硬校验（target_length=0），
// 内存立即可读，落库走节流异步路径。
func TestCaptureOpenAICodexTicket_StoresRealResponseBlob(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, TTLSeconds: 3600}, nil)
	persisted := make(chan struct{}, 4)
	svc.accountRepo = &codexTicketLifecycleRepo{persist: func(context.Context) error {
		select {
		case persisted <- struct{}{}:
		default:
		}
		return nil
	}}
	account := ticketTestAccount(41)

	state := fakeCodexTicketState(312) // 上游漂移后的新格式（降级形状 → 降级槽）
	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, state)
	svc.captureOpenAICodexTicket(account, "gpt-6-astra", h)

	bucket := svc.lookupOpenAICodexTicketBucket(account, "gpt-6-astra")
	require.NotNil(t, bucket)
	require.NotNil(t, bucket.Degraded)
	require.Equal(t, state, bucket.Degraded.State)
	require.Equal(t, 312, bucket.Degraded.Length)
	require.True(t, bucket.Degraded.valid(time.Now(), 0))
	require.Nil(t, bucket.Normal)

	select {
	case <-persisted:
	case <-time.After(2 * time.Second):
		t.Fatal("captured ticket was not persisted")
	}

	// 第二个模型的捕获同样立即进内存；同账号 30s 落库节流不影响读取路径。
	solState := fakeCodexTicketState(292)
	h = http.Header{}
	h.Set(openAICodexTurnStateHeader, solState)
	svc.captureOpenAICodexTicket(account, "gpt-5.6-sol", h)
	sol := svc.lookupOpenAICodexTicketBucket(account, "gpt-5.6-sol")
	require.NotNil(t, sol)
	require.NotNil(t, sol.Normal)
	require.Equal(t, solState, sol.Normal.State)
}

func TestCaptureOpenAICodexTicket_SkipsInvalidInput(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, TTLSeconds: 3600}, nil)
	account := ticketTestAccount(41)

	// 非门控模型不存。
	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, fakeCodexTicketState(312))
	svc.captureOpenAICodexTicket(account, "gpt-5.5", h)
	require.Nil(t, svc.lookupOpenAICodexTicketBucket(account, "gpt-5.5"))

	// 缺 gAAAAA 前缀不存。
	h = http.Header{}
	h.Set(openAICodexTurnStateHeader, strings.Repeat("x", 312))
	svc.captureOpenAICodexTicket(account, "gpt-6-astra", h)
	require.Nil(t, svc.lookupOpenAICodexTicketBucket(account, "gpt-6-astra"))

	// 无头不存。
	svc.captureOpenAICodexTicket(account, "gpt-6-astra", http.Header{})
	require.Nil(t, svc.lookupOpenAICodexTicketBucket(account, "gpt-6-astra"))

	// 严格长度模式（target_length>0）下长度不符不存。
	strict := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, TargetLength: 292, TTLSeconds: 3600}, nil)
	h = http.Header{}
	h.Set(openAICodexTurnStateHeader, fakeCodexTicketState(312))
	strict.captureOpenAICodexTicket(account, "gpt-6-astra", h)
	require.Nil(t, strict.lookupOpenAICodexTicketBucket(account, "gpt-6-astra"))
}

func TestCaptureOpenAICodexTicket_DisabledNoop(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: false, TTLSeconds: 3600}, nil)
	account := ticketTestAccount(41)
	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, fakeCodexTicketState(312))
	svc.captureOpenAICodexTicket(account, "gpt-6-astra", h)
	require.Nil(t, svc.lookupOpenAICodexTicketBucket(account, "gpt-6-astra"))
}

// 观察模式（enabled=true, inject=false）：不再主动打票，一发合成探测都不发。
func TestCodexTicketHarvesterSkipsObserveMode(t *testing.T) {
	upstream := &httpUpstreamRecorder{}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, Inject: false, TTLSeconds: 3600}, upstream)
	account := ticketTestAccount(41)
	account.Status = StatusActive
	svc.accountRepo = &codexTicketLifecycleRepo{account: *account}

	svc.refreshOpenAICodexTickets(context.Background())
	require.Empty(t, upstream.requests)
}

// 演练模式（inject=true, dry_run 默认开）：只观察请求侧决策，同样不发合成探测。
func TestCodexTicketHarvesterSkipsDryRunMode(t *testing.T) {
	upstream := &httpUpstreamRecorder{}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, Inject: true, TTLSeconds: 3600}, upstream)
	account := ticketTestAccount(41)
	account.Status = StatusActive
	svc.accountRepo = &codexTicketLifecycleRepo{account: *account}

	svc.refreshOpenAICodexTickets(context.Background())
	require.Empty(t, upstream.requests)
}

// 管理端票据详情：内存（新）与 extra（旧/过期）按槽合并，每张票含 blob、形状与就绪状态。
func TestOpenAICodexTicketDetails_MergesMemoryAndExtra(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, TTLSeconds: 3600}, nil)
	account := ticketTestAccount(41)
	now := time.Now()

	expired := &openAICodexTicket{
		AccountID: 41, Model: "gpt-5.6-sol",
		State: fakeCodexTicketState(300), Length: 300,
		CapturedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour),
	}
	account.Extra = map[string]any{openAICodexTicketExtraKey("gpt-5.6-sol"): expired}

	fresh := &openAICodexTicket{
		AccountID: 41, Model: "gpt-6-astra",
		State: fakeCodexTicketState(292), Length: 292,
		CapturedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	svc.openaiCodexTickets.Store(openAICodexTicketKey(41, "gpt-6-astra"), &openAICodexTicketBucket{
		AccountID: 41, Model: "gpt-6-astra", Normal: fresh,
	})

	details := svc.OpenAICodexTicketDetails(account, now)
	require.Len(t, details, 2)
	byKey := make(map[string]OpenAICodexTicketDetail, len(details))
	for _, d := range details {
		byKey[d.Model+"/"+d.Shape] = d
	}

	astra := byKey["gpt-6-astra/normal"]
	require.True(t, astra.Ready)
	require.Equal(t, fresh.State, astra.State)
	require.Equal(t, 292, astra.Length)
	require.Positive(t, astra.RemainingSeconds)
	require.NotNil(t, astra.CapturedAt)
	require.NotNil(t, astra.ExpiresAt)

	sol := byKey["gpt-5.6-sol/unknown"]
	require.False(t, sol.Ready)
	require.Equal(t, expired.State, sol.State) // 过期票仍展示记录，只是不就绪
	require.Zero(t, sol.RemainingSeconds)
}

func TestOpenAICodexTicketDetails_DisabledOrNonOAuthNil(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: false}, nil)
	require.Empty(t, svc.OpenAICodexTicketDetails(ticketTestAccount(41), time.Now()))

	svc = ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true}, nil)
	apikeyAccount := &Account{ID: 42, Platform: PlatformOpenAI, Type: "apikey"}
	require.Empty(t, svc.OpenAICodexTicketDetails(apikeyAccount, time.Now()))
}
