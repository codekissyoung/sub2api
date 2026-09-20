package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

const (
	openAICodexTicketExtraKeyPrefix  = "codex_turn_ticket:"
	openAICodexAstraMinVersion       = "0.153.4"
	openAICodexTicketStatePrefix     = "gAAAAA"
	openAICodexTicketDefaultModel    = "gpt-6-astra"
	openAICodexTicketDefaultSolModel = "gpt-5.6-sol"
)

// turn-state blob 的形状分级。blob 是 Fernet token（gAAAAA 前缀 = 0x80 版本
// 字节 + 8 字节签发时间戳），长度反映密文块数：个人号 292=10 块为正常态、
// 312=11 块为降级态（多一块 payload，社区观测口径，非上游公开协议）。
// 只有正常态可注入；降级态与未识别形状仅作证据留存与标记。
const (
	openAICodexTicketShapeNormal   = "normal"
	openAICodexTicketShapeDegraded = "degraded"
	openAICodexTicketShapeUnknown  = "unknown"
)

// 注入决策动作（dry_run 日志与强制执行共用同一决策树）。
const (
	openAICodexTicketActionInject       = "inject"         // 客户端未带 state → 补票
	openAICodexTicketActionReplace      = "replace"        // 客户端带回降级态 → 换正常票
	openAICodexTicketActionKeepClient   = "keep_client"    // 客户端带回正常态 → 不动（它的更新鲜）
	openAICodexTicketActionKeepUnknown  = "keep_unknown"   // 客户端带回未知形状 → 不动（保守）
	openAICodexTicketActionPassNoTicket = "pass_no_ticket" // 无正常票可注 → 放行（fail-open）
)

func openAICodexTicketShapeForLength(length int, cfg config.OpenAICodexTicketConfig) string {
	if length <= 0 {
		return openAICodexTicketShapeUnknown
	}
	normalLen, degradedLen := cfg.NormalLength, cfg.DegradedLength
	if normalLen <= 0 {
		normalLen = 292
	}
	if degradedLen <= 0 {
		degradedLen = 312
	}
	switch length {
	case normalLen:
		return openAICodexTicketShapeNormal
	case degradedLen:
		return openAICodexTicketShapeDegraded
	default:
		return openAICodexTicketShapeUnknown
	}
}

// openAICodexTicketBucket 按形状分槽存一个 (账号, 模型) 的门票。
// 正常/降级槽分离，避免上游在两种形状间抖动时最新的 312 把还能用的 292
// 顶掉；Other 槽留未识别形状作格式漂移证据。map 中的 bucket 一经 Store
// 不再原地修改（clone-on-write），读侧拿到的指针是不可变快照。
type openAICodexTicketBucket struct {
	AccountID int64              `json:"account_id"`
	Model     string             `json:"model"`
	Normal    *openAICodexTicket `json:"normal,omitempty"`
	Degraded  *openAICodexTicket `json:"degraded,omitempty"`
	Other     *openAICodexTicket `json:"other,omitempty"`
}

func (b *openAICodexTicketBucket) clone() *openAICodexTicketBucket {
	if b == nil {
		return nil
	}
	out := *b
	return &out
}

func (b *openAICodexTicketBucket) set(ticket *openAICodexTicket, cfg config.OpenAICodexTicketConfig) {
	if b == nil || ticket == nil {
		return
	}
	switch openAICodexTicketShapeForLength(ticket.Length, cfg) {
	case openAICodexTicketShapeNormal:
		b.Normal = ticket
	case openAICodexTicketShapeDegraded:
		b.Degraded = ticket
	default:
		b.Other = ticket
	}
}

// merge 逐槽取更新的一张（CapturedAt 晚者胜），返回新 bucket，不改原值。
func (b *openAICodexTicketBucket) merge(other *openAICodexTicketBucket) *openAICodexTicketBucket {
	if b == nil {
		return other.clone()
	}
	out := b.clone()
	if other == nil {
		return out
	}
	out.Normal = newerOpenAICodexTicket(out.Normal, other.Normal)
	out.Degraded = newerOpenAICodexTicket(out.Degraded, other.Degraded)
	out.Other = newerOpenAICodexTicket(out.Other, other.Other)
	if out.AccountID == 0 {
		out.AccountID = other.AccountID
	}
	if out.Model == "" {
		out.Model = other.Model
	}
	return out
}

func newerOpenAICodexTicket(a, b *openAICodexTicket) *openAICodexTicket {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if b.CapturedAt.After(a.CapturedAt) {
		return b
	}
	return a
}

// 同桶并发合并（同号同模型并行请求的响应同时捕获）用分段锁保护。
var openAICodexTicketBucketLocks [64]sync.Mutex

func openAICodexTicketBucketLock(key string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return &openAICodexTicketBucketLocks[h.Sum32()%uint32(len(openAICodexTicketBucketLocks))]
}

// ErrOpenAICodexTicketUnavailable 表示该号该模型没有可用的 292 门票，
// 且 fail_closed 禁止裸打业务请求。
var ErrOpenAICodexTicketUnavailable = errors.New("codex turn-state ticket unavailable")

type openAICodexTicket struct {
	AccountID  int64     `json:"account_id"`
	Model      string    `json:"model"`
	State      string    `json:"state"`
	Length     int       `json:"length"`
	CapturedAt time.Time `json:"captured_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Attempts   int       `json:"attempts"`
}

func openAICodexTicketKey(accountID int64, model string) string {
	return fmt.Sprintf("%d\x00%s", accountID, strings.TrimSpace(model))
}

func openAICodexTicketExtraKey(model string) string {
	return openAICodexTicketExtraKeyPrefix + strings.TrimSpace(model)
}

func normalizeOpenAICodexTicketModel(model string) string {
	return strings.TrimSpace(model)
}

func extractOpenAICodexTicketModel(body []byte) string {
	return normalizeOpenAICodexTicketModel(gjson.GetBytes(body, "model").String())
}

func (s *OpenAIGatewayService) openAICodexTicketConfig() config.OpenAICodexTicketConfig {
	cfg := config.OpenAICodexTicketConfig{}
	if s != nil && s.cfg != nil {
		cfg = s.cfg.Gateway.OpenAICodexTicket
	}
	if cfg.TTLSeconds <= 0 {
		cfg.TTLSeconds = 3600
	}
	if cfg.RefreshBeforeSeconds <= 0 {
		cfg.RefreshBeforeSeconds = 600
	}
	if cfg.HarvestProbeIntervalSeconds <= 0 {
		cfg.HarvestProbeIntervalSeconds = 6
	}
	if cfg.HarvestAttemptTimeoutSeconds <= 0 {
		cfg.HarvestAttemptTimeoutSeconds = 25
	}
	if len(cfg.Models) == 0 {
		cfg.Models = []string{openAICodexTicketDefaultModel, openAICodexTicketDefaultSolModel}
	}
	if cfg.NormalLength <= 0 {
		cfg.NormalLength = 292
	}
	if cfg.DegradedLength <= 0 {
		cfg.DegradedLength = 312
	}
	if cfg.InjectMinRemainingSeconds <= 0 {
		cfg.InjectMinRemainingSeconds = 600
	}
	return cfg
}

func (s *OpenAIGatewayService) openAICodexTicketGatedModel(model string) bool {
	model = normalizeOpenAICodexTicketModel(model)
	if model == "" || !s.openAICodexTicketEnabled() {
		return false
	}
	for _, item := range s.openAICodexTicketConfig().Models {
		if normalizeOpenAICodexTicketModel(item) == model {
			return true
		}
	}
	return false
}

// OpenAICodexTicketStatus 是给管理端看的门票摘要，不含 state blob。
type OpenAICodexTicketStatus struct {
	Model            string     `json:"model"`
	Shape            string     `json:"shape,omitempty"`
	Length           int        `json:"length,omitempty"`
	Ready            bool       `json:"ready"`
	Degraded         bool       `json:"degraded,omitempty"`
	RemainingSeconds int64      `json:"remaining_seconds"`
	Blocked          bool       `json:"blocked"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
}

func OpenAICodexTicketStatuses(account *Account, cfg config.OpenAICodexTicketConfig, now time.Time) []OpenAICodexTicketStatus {
	if !cfg.Enabled || !isOpenAICodexTicketAccount(account) {
		return nil
	}
	models := cfg.Models
	if len(models) == 0 {
		models = []string{openAICodexTicketDefaultModel, openAICodexTicketDefaultSolModel}
	}
	if cfg.NormalLength <= 0 {
		cfg.NormalLength = 292
	}
	if cfg.DegradedLength <= 0 {
		cfg.DegradedLength = 312
	}
	out := make([]OpenAICodexTicketStatus, 0, len(models))
	for _, model := range models {
		model = normalizeOpenAICodexTicketModel(model)
		if model == "" {
			continue
		}
		status := OpenAICodexTicketStatus{Model: model}
		var bucket *openAICodexTicketBucket
		if account != nil && account.Extra != nil {
			bucket = parseOpenAICodexTicketBucketFromAny(account.ID, model, account.Extra[openAICodexTicketExtraKey(model)], cfg)
		}
		if bucket != nil {
			// 展示槽位优先级：正常票有效 → normal；否则降级票有效 → degraded
			// （该号当前被上游降级的标记）；再否则未知形状；全无效 → 最近一张。
			display, shape := pickOpenAICodexTicketDisplaySlot(bucket, now)
			status.Shape = shape
			if display != nil {
				status.Length = display.Length
				exp := display.ExpiresAt
				status.ExpiresAt = &exp
				if remaining := int64(display.ExpiresAt.Sub(now) / time.Second); remaining > 0 {
					status.RemainingSeconds = remaining
				}
			}
			if shape == openAICodexTicketShapeNormal && bucket.Normal.valid(now, 0) {
				status.Ready = true
			}
			status.Degraded = shape == openAICodexTicketShapeDegraded
		}
		status.Blocked = cfg.FailClosed && cfg.Inject && !status.Ready
		out = append(out, status)
	}
	return out
}

// pickOpenAICodexTicketDisplaySlot 选出管理端摘要要展示的槽位。
func pickOpenAICodexTicketDisplaySlot(bucket *openAICodexTicketBucket, now time.Time) (*openAICodexTicket, string) {
	if bucket == nil {
		return nil, ""
	}
	if bucket.Normal != nil && bucket.Normal.valid(now, 0) {
		return bucket.Normal, openAICodexTicketShapeNormal
	}
	if bucket.Degraded != nil && bucket.Degraded.valid(now, 0) {
		return bucket.Degraded, openAICodexTicketShapeDegraded
	}
	if bucket.Other != nil && bucket.Other.valid(now, 0) {
		return bucket.Other, openAICodexTicketShapeUnknown
	}
	latest := newerOpenAICodexTicket(newerOpenAICodexTicket(bucket.Normal, bucket.Degraded), bucket.Other)
	if latest == nil {
		return nil, ""
	}
	return latest, openAICodexTicketShapeForLength(latest.Length, config.OpenAICodexTicketConfig{})
}

// OpenAICodexTicketDetail 是管理端「票据」视图用的完整门票信息，包含 state blob。
// 仅经 admin 鉴权接口暴露：blob 是不透明回合状态而非凭证，1 小时自然过期，
// 但仍属上游铸造的敏感材料——不写入日志、不进入导出（RedactOpenAICodexTicketExtra）。
type OpenAICodexTicketDetail struct {
	Model            string     `json:"model"`
	Shape            string     `json:"shape,omitempty"`
	State            string     `json:"state,omitempty"`
	Length           int        `json:"length,omitempty"`
	Ready            bool       `json:"ready"`
	RemainingSeconds int64      `json:"remaining_seconds"`
	CapturedAt       *time.Time `json:"captured_at,omitempty"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
}

// OpenAICodexTicketDetails 合并内存与落库两份票（lookupOpenAICodexTicketBucket 已做
// 逐槽新旧裁决），按门控模型逐槽返回；无记录的模型返回占位行（Ready=false）。
// 一个模型最多三行：normal（可注入）/ degraded（降级标记）/ unknown（漂移证据）。
func (s *OpenAIGatewayService) OpenAICodexTicketDetails(account *Account, now time.Time) []OpenAICodexTicketDetail {
	if s == nil || !s.openAICodexTicketEnabled() || !isOpenAICodexTicketAccount(account) {
		return nil
	}
	cfg := s.openAICodexTicketConfig()
	models := cfg.Models
	if len(models) == 0 {
		models = []string{openAICodexTicketDefaultModel, openAICodexTicketDefaultSolModel}
	}
	out := make([]OpenAICodexTicketDetail, 0, len(models))
	appendSlot := func(model, shape string, ticket *openAICodexTicket) {
		detail := OpenAICodexTicketDetail{Model: model, Shape: shape}
		if ticket != nil {
			detail.State = ticket.State
			detail.Length = ticket.Length
			captured := ticket.CapturedAt
			detail.CapturedAt = &captured
			exp := ticket.ExpiresAt
			detail.ExpiresAt = &exp
			if remaining := int64(ticket.ExpiresAt.Sub(now) / time.Second); remaining > 0 {
				detail.RemainingSeconds = remaining
			}
			// Ready 仅授予「正常形状且未过期」——只有它能进注入候选。
			detail.Ready = shape == openAICodexTicketShapeNormal && ticket.valid(now, 0)
		}
		out = append(out, detail)
	}
	for _, model := range models {
		model = normalizeOpenAICodexTicketModel(model)
		if model == "" {
			continue
		}
		bucket := s.lookupOpenAICodexTicketBucket(account, model)
		if bucket == nil || (bucket.Normal == nil && bucket.Degraded == nil && bucket.Other == nil) {
			out = append(out, OpenAICodexTicketDetail{Model: model})
			continue
		}
		if bucket.Normal != nil {
			appendSlot(model, openAICodexTicketShapeNormal, bucket.Normal)
		}
		if bucket.Degraded != nil {
			appendSlot(model, openAICodexTicketShapeDegraded, bucket.Degraded)
		}
		if bucket.Other != nil {
			appendSlot(model, openAICodexTicketShapeUnknown, bucket.Other)
		}
	}
	return out
}

func (s *OpenAIGatewayService) openAICodexTicketEnabled() bool {
	return s.openAICodexTicketEnabledContext(context.Background())
}

func (s *OpenAIGatewayService) openAICodexTicketEnabledContext(ctx context.Context) bool {
	if s == nil {
		return false
	}
	fallback := s.cfg != nil && s.cfg.Gateway.OpenAICodexTicket.Enabled
	if s.settingService != nil {
		return s.settingService.GetOpenAICodexTicketEnabled(ctx, fallback)
	}
	return fallback
}

// openAICodexTicketInjectEnabledContext 返回是否把门票注入业务请求。
// 关闭即观察模式：打票照常，出站不注入、调度不拦截。
func (s *OpenAIGatewayService) openAICodexTicketInjectEnabled() bool {
	return s.openAICodexTicketInjectEnabledContext(context.Background())
}

func (s *OpenAIGatewayService) openAICodexTicketInjectEnabledContext(ctx context.Context) bool {
	if s == nil {
		return false
	}
	fallback := s.cfg != nil && s.cfg.Gateway.OpenAICodexTicket.Inject
	if s.settingService != nil {
		return s.settingService.GetOpenAICodexTicketInjectEnabled(ctx, fallback)
	}
	return fallback
}

func (s *OpenAIGatewayService) openAICodexTicketHarvestProxyURL() string {
	return s.openAICodexTicketHarvestProxyURLContext(context.Background())
}

func (s *OpenAIGatewayService) openAICodexTicketHarvestProxyURLContext(ctx context.Context) string {
	if s.settingService != nil {
		if proxy := s.settingService.GetOpenAICodexTicketHarvestProxyURL(ctx); proxy != "" {
			return proxy
		}
	}
	return strings.TrimSpace(s.openAICodexTicketConfig().HarvestProxyURL)
}

func (t *openAICodexTicket) valid(now time.Time, targetLen int) bool {
	if t == nil {
		return false
	}
	state := strings.TrimSpace(t.State)
	if state == "" || !strings.HasPrefix(state, openAICodexTicketStatePrefix) {
		return false
	}
	// targetLen<=0 不校验长度：上游 blob 格式会漂移（292 一夜变 312），
	// 被动捕获到的真实票不应因长度硬编码而报废。
	if targetLen > 0 && (len(state) != targetLen || t.Length != targetLen) {
		return false
	}
	if t.ExpiresAt.IsZero() || !now.Before(t.ExpiresAt) {
		return false
	}
	return true
}

func (t *openAICodexTicket) needsRefresh(now time.Time, refreshBefore time.Duration) bool {
	if t == nil || t.ExpiresAt.IsZero() {
		return true
	}
	return !t.ExpiresAt.After(now.Add(refreshBefore))
}

// lookupOpenAICodexTicketBucket 合并内存与落库两份 bucket（逐槽取新），
// 合并结果回写内存。返回的 bucket 是不可变快照，读侧无需加锁。
func (s *OpenAIGatewayService) lookupOpenAICodexTicketBucket(account *Account, model string) *openAICodexTicketBucket {
	if s == nil || account == nil || account.ID <= 0 {
		return nil
	}
	model = normalizeOpenAICodexTicketModel(model)
	if model == "" {
		return nil
	}
	key := openAICodexTicketKey(account.ID, model)
	cfg := s.openAICodexTicketConfig()
	var extra *openAICodexTicketBucket
	if account.Extra != nil {
		extra = parseOpenAICodexTicketBucketFromAny(account.ID, model, account.Extra[openAICodexTicketExtraKey(model)], cfg)
	}
	mu := openAICodexTicketBucketLock(key)
	mu.Lock()
	defer mu.Unlock()
	var mem *openAICodexTicketBucket
	if raw, ok := s.openaiCodexTickets.Load(key); ok {
		mem, _ = raw.(*openAICodexTicketBucket)
	}
	merged := mem.merge(extra)
	if merged == nil {
		return nil
	}
	merged.AccountID = account.ID
	merged.Model = model
	s.openaiCodexTickets.Store(key, merged)
	return merged
}

// parseOpenAICodexTicketBucketFromAny 解析 extra 中的 bucket；兼容旧的扁平
// 单票格式（{"state": ...}，按长度分级归入对应槽位）。
func parseOpenAICodexTicketBucketFromAny(accountID int64, model string, raw any, cfg config.OpenAICodexTicketConfig) *openAICodexTicketBucket {
	if raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil
	}
	model = normalizeOpenAICodexTicketModel(model)
	if _, flat := probe["state"]; flat {
		ticket := parseOpenAICodexTicketFromAny(accountID, model, raw)
		if ticket == nil {
			return nil
		}
		bucket := &openAICodexTicketBucket{AccountID: accountID, Model: model}
		bucket.set(ticket, cfg)
		return bucket
	}
	var bucket openAICodexTicketBucket
	if err := json.Unmarshal(b, &bucket); err != nil {
		return nil
	}
	bucket.AccountID = accountID
	if model != "" {
		bucket.Model = model
	}
	for _, slot := range []*openAICodexTicket{bucket.Normal, bucket.Degraded, bucket.Other} {
		if slot == nil {
			continue
		}
		slot.AccountID = accountID
		if model != "" {
			slot.Model = model
		}
		slot.State = strings.TrimSpace(slot.State)
		if slot.Length == 0 {
			slot.Length = len(slot.State)
		}
	}
	if bucket.Normal == nil && bucket.Degraded == nil && bucket.Other == nil {
		return nil
	}
	return &bucket
}

func parseOpenAICodexTicketFromAny(accountID int64, model string, raw any) *openAICodexTicket {
	if raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var ticket openAICodexTicket
	if err := json.Unmarshal(b, &ticket); err != nil {
		return nil
	}
	ticket.AccountID = accountID
	if strings.TrimSpace(model) != "" {
		ticket.Model = model
	}
	ticket.State = strings.TrimSpace(ticket.State)
	if ticket.Length == 0 {
		ticket.Length = len(ticket.State)
	}
	if ticket.State == "" {
		return nil
	}
	return &ticket
}

// mergeOpenAICodexTicketIntoBucket 把一张新票按形状归入桶（clone-on-write），
// 返回合并后的不可变快照。
func (s *OpenAIGatewayService) mergeOpenAICodexTicketIntoBucket(account *Account, ticket *openAICodexTicket) *openAICodexTicketBucket {
	if s == nil || account == nil || ticket == nil || account.ID <= 0 {
		return nil
	}
	model := normalizeOpenAICodexTicketModel(ticket.Model)
	if model == "" {
		return nil
	}
	ticket.Model = model
	ticket.AccountID = account.ID
	cfg := s.openAICodexTicketConfig()
	key := openAICodexTicketKey(account.ID, model)
	mu := openAICodexTicketBucketLock(key)
	mu.Lock()
	defer mu.Unlock()
	var bucket *openAICodexTicketBucket
	if raw, ok := s.openaiCodexTickets.Load(key); ok {
		bucket, _ = raw.(*openAICodexTicketBucket)
	}
	if bucket == nil {
		bucket = &openAICodexTicketBucket{AccountID: account.ID, Model: model}
	} else {
		bucket = bucket.clone()
	}
	bucket.set(ticket, cfg)
	s.openaiCodexTickets.Store(key, bucket)
	return bucket
}

func (s *OpenAIGatewayService) storeOpenAICodexTicket(ctx context.Context, account *Account, ticket *openAICodexTicket) {
	bucket := s.mergeOpenAICodexTicketIntoBucket(account, ticket)
	if bucket == nil || s.accountRepo == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
		openAICodexTicketExtraKey(bucket.Model): bucket,
	}); err != nil {
		logger.L().Warn("openai_codex_ticket persist failed",
			zap.Int64("account_id", account.ID),
			zap.String("model", bucket.Model),
			zap.Error(err),
		)
	}
}

// captureOpenAICodexTicket 从真实业务响应头被动捕获 turn-state 门票并落存。
// 上游在每个成功响应里铸造/轮换该 blob（正版 Codex 客户端也是这么收票的），
// 所以这里零额外上游流量。内存立即更新；落库按账号节流并异步进行（与 codex
// usage 快照同一模式），避免给每条业务请求叠加一次同步写。
// outboundModel 必须是真正出站的模型名，与注入侧 openAICodexTicketOutboundModel
// 口径一致；仅捕获门控模型列表内的票。必须在响应已提交（不再 failover）的
// 成功路径调用。
func (s *OpenAIGatewayService) captureOpenAICodexTicket(account *Account, outboundModel string, upstream http.Header) {
	if s == nil || !isOpenAICodexTicketAccount(account) || !s.openAICodexTicketEnabled() {
		return
	}
	model := normalizeOpenAICodexTicketModel(outboundModel)
	if model == "" || !s.openAICodexTicketGatedModel(model) {
		return
	}
	state := extractOpenAICodexTurnState(upstream)
	if state == "" || !strings.HasPrefix(state, openAICodexTicketStatePrefix) {
		return
	}
	cfg := s.openAICodexTicketConfig()
	if cfg.TargetLength > 0 && len(state) != cfg.TargetLength {
		return
	}
	now := time.Now()
	ticket := &openAICodexTicket{
		AccountID:  account.ID,
		Model:      model,
		State:      state,
		Length:     len(state),
		CapturedAt: now,
		ExpiresAt:  now.Add(time.Duration(cfg.TTLSeconds) * time.Second),
	}
	s.mergeOpenAICodexTicketIntoBucket(account, ticket)
	if s.accountRepo == nil || !s.getCodexTicketPersistThrottle().Allow(account.ID, now) {
		return
	}
	key := openAICodexTicketKey(account.ID, model)
	go func() {
		// 落库取当前内存里的整桶快照（三槽），而非单票：并发捕获的另一形状
		// 不应被这次写覆盖。
		raw, ok := s.openaiCodexTickets.Load(key)
		if !ok {
			return
		}
		bucket, _ := raw.(*openAICodexTicketBucket)
		if bucket == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
			openAICodexTicketExtraKey(model): bucket,
		}); err != nil {
			logger.L().Warn("openai_codex_ticket persist failed",
				zap.Int64("account_id", account.ID),
				zap.String("model", model),
				zap.Error(err),
			)
		}
	}()
}

// injectableOpenAICodexTicket 返回可注入的正常形状门票：未过期且剩余有效期
// 超过 inject_min_remaining_seconds 安全边际（名义 TTL 1h，按捕获时间保守估）。
func (s *OpenAIGatewayService) injectableOpenAICodexTicket(account *Account, model string, now time.Time, cfg config.OpenAICodexTicketConfig) *openAICodexTicket {
	bucket := s.lookupOpenAICodexTicketBucket(account, model)
	if bucket == nil || bucket.Normal == nil {
		return nil
	}
	ticket := bucket.Normal
	if !ticket.valid(now, 0) {
		return nil
	}
	margin := cfg.InjectMinRemainingSeconds
	if margin <= 0 {
		margin = 600
	}
	if !ticket.ExpiresAt.After(now.Add(time.Duration(margin) * time.Second)) {
		return nil
	}
	return ticket
}

// decideOpenAICodexTicketAction 注入决策树（dry_run 与强制执行共用）：
// 客户端带回正常态 → 不动（它的票比库里的新鲜）；带回降级态 → 替换；
// 未带 → 补票；未知形状 → 保守不动；无正常票 → 放行（fail-open）。
func decideOpenAICodexTicketAction(ticket *openAICodexTicket, clientState string, cfg config.OpenAICodexTicketConfig) string {
	if ticket == nil {
		return openAICodexTicketActionPassNoTicket
	}
	clientState = strings.TrimSpace(clientState)
	if clientState == "" {
		return openAICodexTicketActionInject
	}
	switch openAICodexTicketShapeForLength(len(clientState), cfg) {
	case openAICodexTicketShapeNormal:
		return openAICodexTicketActionKeepClient
	case openAICodexTicketShapeDegraded:
		return openAICodexTicketActionReplace
	default:
		return openAICodexTicketActionKeepUnknown
	}
}

// openAICodexTicketInjectDryRunContext 返回注入是否为演练模式。
// 演练模式跑完整决策树但只记日志、不改写请求头。默认 true（安全）：
// settings 键缺失时一律按演练处理。
func (s *OpenAIGatewayService) openAICodexTicketInjectDryRun(ctx context.Context) bool {
	if s == nil {
		return true
	}
	if s.settingService != nil {
		return s.settingService.GetOpenAICodexTicketInjectDryRun(ctx, true)
	}
	return true
}

func (s *OpenAIGatewayService) logOpenAICodexTicketDecision(account *Account, model, action, clientState string, ticket *openAICodexTicket, now time.Time, dryRun bool) {
	fields := []zap.Field{
		zap.Int64("account_id", account.ID),
		zap.String("model", model),
		zap.String("action", action),
		zap.Int("client_len", len(strings.TrimSpace(clientState))),
		zap.Bool("dry_run", dryRun),
	}
	if ticket != nil {
		fields = append(fields,
			zap.Int("ticket_len", ticket.Length),
			zap.Int64("ticket_remaining_sec", int64(ticket.ExpiresAt.Sub(now)/time.Second)),
		)
	}
	logger.L().Info("openai_codex_ticket inject decision", fields...)
}

// applyOpenAICodexTicket 按决策树处理出站请求的 x-codex-turn-state。
// 演练模式（inject_dry_run，默认开）跑完整决策树只记日志不改写；
// 正式模式只注入正常形状（292）且余期充足的票，无票 fail-open
// （FailClosed 例外，默认关）。票的第一来源是真实响应的被动捕获
// （captureOpenAICodexTicket），后台 harvester 仅 inject 模式下兜底。
func (s *OpenAIGatewayService) applyOpenAICodexTicket(ctx context.Context, account *Account, model string, h http.Header) error {
	if s == nil || h == nil || !isOpenAICodexTicketAccount(account) || !s.openAICodexTicketEnabledContext(ctx) || !s.openAICodexTicketInjectEnabledContext(ctx) {
		return nil
	}
	model = normalizeOpenAICodexTicketModel(model)
	if model == "" || !s.openAICodexTicketGatedModel(model) {
		return nil
	}
	cfg := s.openAICodexTicketConfig()
	now := time.Now()
	ticket := s.injectableOpenAICodexTicket(account, model, now, cfg)
	clientState := strings.TrimSpace(h.Get(openAICodexTurnStateHeader))
	action := decideOpenAICodexTicketAction(ticket, clientState, cfg)
	dryRun := s.openAICodexTicketInjectDryRun(ctx)
	if dryRun {
		s.logOpenAICodexTicketDecision(account, model, action, clientState, ticket, now, true)
		return nil
	}
	switch action {
	case openAICodexTicketActionInject, openAICodexTicketActionReplace:
		h.Set(openAICodexTurnStateHeader, ticket.State)
		s.logOpenAICodexTicketDecision(account, model, action, clientState, ticket, now, false)
		return nil
	case openAICodexTicketActionPassNoTicket:
		if cfg.FailClosed {
			return ErrOpenAICodexTicketUnavailable
		}
	}
	return nil
}

// openAICodexTicketOutboundModel 预测本请求真正出站的模型名，也就是
// applyOpenAICodexTicket 注入时读到的 body.model。
//
// 调度门控与注入必须按同一个模型名判定门票。普通请求下二者同源：Forward 的
// upstreamModel 与本函数都走 resolveOpenAIAccountUpstreamModelForRequest，且
// Forward 会把 body.model 改写成该值后才注入。但 /responses/compact 例外——
// Forward 会把出站模型进一步改写为 compact 映射或 gateway.openai_compact_model
// （默认非空），此时若门控仍按客户端原始模型判定，就会把「实际出站是非门控
// 模型、根本不需要票」的 compact 请求整片误拦成不可调度。
func (s *OpenAIGatewayService) openAICodexTicketOutboundModel(account *Account, requestedModel string, requireCompact bool) string {
	model := strings.TrimSpace(requestedModel)
	if account == nil || model == "" {
		return model
	}
	if !account.IsOpenAI() {
		return canonicalOpenAIAccountSchedulingModel(account, model)
	}
	_, upstreamModel := resolveOpenAIForwardMappedModels(account, model, requireCompact)
	if requireCompact {
		// 与 Forward 同序：compact 兜底模型优先于普通/compact 映射结果。
		if compactModel := strings.TrimSpace(s.resolveOpenAICompactFallbackModel(account, model)); compactModel != "" {
			upstreamModel = compactModel
		}
	}
	if upstreamModel = strings.TrimSpace(upstreamModel); upstreamModel != "" {
		return upstreamModel
	}
	return model
}

// outboundModel 必须是真正会发给上游的模型名（openAICodexTicketOutboundModel），
// 不是客户端原始模型：注入侧读的是出站 body.model，两侧口径必须一致。
func (s *OpenAIGatewayService) openAICodexTicketBlocksAccount(account *Account, outboundModel string) bool {
	if s == nil || !isOpenAICodexTicketAccount(account) || !s.openAICodexTicketEnabled() {
		return false
	}
	if !s.openAICodexTicketInjectEnabled() {
		return false
	}
	// 演练模式只做决策日志，不改变调度：fail_closed 门控一并抑制。
	if s.openAICodexTicketInjectDryRun(context.Background()) {
		return false
	}
	cfg := s.openAICodexTicketConfig()
	if !cfg.FailClosed {
		return false
	}
	model := normalizeOpenAICodexTicketModel(outboundModel)
	if !s.openAICodexTicketGatedModel(model) {
		return false
	}
	return s.injectableOpenAICodexTicket(account, model, time.Now(), cfg) == nil
}

func (s *OpenAIGatewayService) fireOpenAICodexTicketProbe(ctx context.Context, account *Account, token, model, proxyURL string, attemptTimeout time.Duration) (state string, status int, err error) {
	attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
	defer cancel()

	body := []byte(`{"model":` + jsonString(model) + `,"store":false,"stream":true,"instructions":"Reply with exactly: pong","input":[{"role":"user","content":[{"type":"input_text","text":"ping"}]}]}`)
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, chatgptCodexURL, bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAIHarvest))
	req.Close = true
	req.Host = "chatgpt.com"
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("session_id", uuid.NewString())
	if err := resolveAndSetOpenAIChatGPTAccountHeaders(attemptCtx, s.accountRepo, req.Header, account); err != nil {
		return "", 0, err
	}
	applyOpenAICodexTicketHarvestIdentity(req.Header, model)

	// Synthetic probes must use the dedicated no-reuse transport even when the
	// production account is bound to a plugin. This also avoids reading pluginManager
	// while handlers are still wiring it during gateway construction.
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		return "", 0, err
	}
	if resp == nil {
		return "", 0, errors.New("nil upstream response")
	}
	// Only the response header is needed; no connection will be reused.
	defer func() {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	return extractOpenAICodexTurnState(resp.Header), resp.StatusCode, nil
}

func jsonString(v string) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `""`
	}
	return string(b)
}

func applyOpenAICodexTicketHarvestIdentity(h http.Header, model string) {
	ensureCodexIdentityHeaders(h)
	enforceCodexIdentityHeaders(h)
	version := strings.TrimSpace(h.Get("version"))
	if needsOpenAICodexAstraVersion(model) && (version == "" || CompareVersions(version, openAICodexAstraMinVersion) < 0) {
		h.Set("version", openAICodexAstraMinVersion)
		h.Set("user-agent", buildCodexCLIUserAgent(openAICodexAstraMinVersion))
		h.Set("originator", openai.CodexDefaultOriginator)
	}
}

func needsOpenAICodexAstraVersion(model string) bool {
	m := strings.ToLower(normalizeOpenAICodexTicketModel(model))
	return strings.Contains(m, "gpt-6") || strings.Contains(m, "astra")
}

func (s *OpenAIGatewayService) StartOpenAICodexTicketHarvester() {
	if s == nil {
		return
	}
	s.openaiCodexTicketLifecycleMu.Lock()
	defer s.openaiCodexTicketLifecycleMu.Unlock()
	if s.openaiCodexTicketStopped || s.openaiCodexTicketDone != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.openaiCodexTicketCancel = cancel
	s.openaiCodexTicketDone = done
	go func() {
		defer close(done)
		s.openAICodexTicketHarvestLoop(ctx)
	}()
	logger.L().Info("openai_codex_ticket harvester started",
		zap.Int("ttl_seconds", s.openAICodexTicketConfig().TTLSeconds),
		zap.Int("target_length", s.openAICodexTicketConfig().TargetLength),
		zap.Strings("models", s.openAICodexTicketConfig().Models),
	)
}

func (s *OpenAIGatewayService) StopOpenAICodexTicketHarvester() {
	if s == nil {
		return
	}
	s.openaiCodexTicketLifecycleMu.Lock()
	s.openaiCodexTicketStopped = true
	cancel, done := s.openaiCodexTicketCancel, s.openaiCodexTicketDone
	s.openaiCodexTicketLifecycleMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (s *OpenAIGatewayService) openAICodexTicketHarvestLoop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.refreshOpenAICodexTickets(ctx)
			timer.Reset(time.Duration(s.openAICodexTicketConfig().HarvestProbeIntervalSeconds) * time.Second)
		}
	}
}

// refreshOpenAICodexTickets probes each account/model with a missing or soon-to-expire
// ticket once. The loop waits for all probes, then waits the configured interval
// before starting the next cycle.
func (s *OpenAIGatewayService) refreshOpenAICodexTickets(ctx context.Context) {
	if s == nil || s.accountRepo == nil || ctx.Err() != nil || !s.openAICodexTicketEnabledContext(ctx) {
		return
	}
	// 主动打票只在 inject 模式运行：观察模式（inject=false）仅靠真实响应
	// 被动捕获攒票，不向 chatgpt.com 发任何合成探测（2026-09-20 空转事故：
	// 上游 blob 变长后硬校验全部 miss，每号每小时数百发合成 ping 白打还吃 429）。
	if !s.openAICodexTicketInjectEnabledContext(ctx) {
		return
	}
	// 演练模式同样不主动打票：dry_run 只观察请求侧决策，合成探测零发送。
	if s.openAICodexTicketInjectDryRun(ctx) {
		return
	}
	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		logger.L().Warn("openai_codex_ticket list accounts failed", zap.Error(err))
		return
	}
	cfg := s.openAICodexTicketConfig()
	now := time.Now()
	refreshBefore := time.Duration(cfg.RefreshBeforeSeconds) * time.Second
	var wg sync.WaitGroup
	probed := 0
	for i := range accounts {
		account := accounts[i]
		if account.Status != StatusActive || !isOpenAICodexTicketAccount(&account) {
			continue
		}
		for _, model := range cfg.Models {
			model := normalizeOpenAICodexTicketModel(model)
			if model == "" {
				continue
			}
			// 已有一张有效且未临近过期的正常票 → 本周期不打，省得白刷。
			if t := s.injectableOpenAICodexTicket(&account, model, now, cfg); t != nil && !t.needsRefresh(now, refreshBefore) {
				continue
			}
			acc := account
			// Token/header helpers may update account metadata; each model owns its maps.
			acc.Extra = maps.Clone(account.Extra)
			acc.Credentials = maps.Clone(account.Credentials)
			probed++
			wg.Add(1)
			go func(acc Account, model string) {
				defer wg.Done()
				s.probeOnceOpenAICodexTicket(ctx, &acc, model)
			}(acc, model)
		}
	}
	wg.Wait()
	if probed > 0 {
		logger.L().Info("openai_codex_ticket probe cycle", zap.Int("probed", probed))
	}
}

// probeOnceOpenAICodexTicket 打一发探测（有打票代理走代理，为空则本机直连）。
// 命中合格 292（HTTP 200、长度==target、
// gAAAAA 前缀）就落库；否则记 Info miss，交给下个周期重试。同一 key 并发去重，避免上一发还没
// 回来又叠一发。
func (s *OpenAIGatewayService) probeOnceOpenAICodexTicket(ctx context.Context, account *Account, model string) {
	if s == nil || !isOpenAICodexTicketAccount(account) || ctx.Err() != nil || !s.openAICodexTicketEnabledContext(ctx) {
		return
	}
	cfg := s.openAICodexTicketConfig()
	proxyURL := s.openAICodexTicketHarvestProxyURLContext(ctx)
	if s.httpUpstream == nil || ctx.Err() != nil {
		return
	}
	key := openAICodexTicketKey(account.ID, model)
	_, _, _ = s.openaiCodexTicketFlight.Do(key, func() (any, error) {
		token, _, err := s.GetAccessToken(ctx, account)
		if err != nil || strings.TrimSpace(token) == "" {
			logger.L().Info("openai_codex_ticket probe miss",
				zap.Int64("account_id", account.ID), zap.String("model", model),
				zap.String("reason", "token"), zap.Error(err))
			return nil, nil
		}
		state, status, perr := s.fireOpenAICodexTicketProbe(ctx, account, token, model, proxyURL, time.Duration(cfg.HarvestAttemptTimeoutSeconds)*time.Second)
		if perr != nil {
			logger.L().Info("openai_codex_ticket probe miss",
				zap.Int64("account_id", account.ID), zap.String("model", model),
				zap.String("reason", "error"), zap.Error(perr))
			return nil, nil
		}
		if status != http.StatusOK || state == "" || (cfg.TargetLength > 0 && len(state) != cfg.TargetLength) || !strings.HasPrefix(state, openAICodexTicketStatePrefix) {
			logger.L().Info("openai_codex_ticket probe miss",
				zap.Int64("account_id", account.ID), zap.String("model", model),
				zap.Int("http", status), zap.Int("len", len(state)))
			return nil, nil
		}
		now := time.Now()
		ticket := &openAICodexTicket{
			AccountID:  account.ID,
			Model:      model,
			State:      state,
			Length:     len(state),
			CapturedAt: now,
			ExpiresAt:  now.Add(time.Duration(cfg.TTLSeconds) * time.Second),
			Attempts:   1,
		}
		s.storeOpenAICodexTicket(ctx, account, ticket)
		logger.L().Info("openai_codex_ticket harvested",
			zap.Int64("account_id", account.ID), zap.String("model", model),
			zap.Int("length", ticket.Length), zap.String("mode", "continuous"))
		return nil, nil
	})
}

// IsOpenAICodexTicketExtraKey identifies server-managed ticket material.
func IsOpenAICodexTicketExtraKey(key string) bool {
	return strings.HasPrefix(key, openAICodexTicketExtraKeyPrefix)
}

// MergeOpenAICodexTicketExtra preserves only persisted tickets, never summaries or
// blobs supplied by an account edit. The repository repeats this under the row
// lock so a concurrent harvest cannot be overwritten by a stale admin snapshot.
func MergeOpenAICodexTicketExtra(extra, current map[string]any) map[string]any {
	result := maps.Clone(extra)
	for key := range result {
		if IsOpenAICodexTicketExtraKey(key) {
			delete(result, key)
		}
	}
	for key, value := range current {
		if IsOpenAICodexTicketExtraKey(key) {
			if result == nil {
				result = make(map[string]any)
			}
			result[key] = value
		}
	}
	return result
}

// ValidateOpenAICodexTicketHarvestProxyURL validates only syntax, without making
// a network request or including credentials in validation errors.
func ValidateOpenAICodexTicketHarvestProxyURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return errors.New("harvest proxy must be an HTTP(S) or SOCKS5(h) URL with a host and no path, query or fragment")
	}
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return errors.New("harvest proxy scheme must be http, https, socks5 or socks5h")
	}
	if port := parsed.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("harvest proxy port must be between 1 and 65535")
		}
	}
	return nil
}

// MaskProxyURL never returns a stored proxy password, even for invalid legacy data.
func MaskProxyURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || ValidateOpenAICodexTicketHarvestProxyURL(raw) != nil {
		return ""
	}
	parsed, _ := url.Parse(raw)
	if parsed.User != nil {
		if _, ok := parsed.User.Password(); ok {
			parsed.User = url.UserPassword(parsed.User.Username(), "***")
		}
	}
	return parsed.String()
}

// IsMaskedProxyURL recognizes the exact password placeholder emitted by the API.
func IsMaskedProxyURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return false
	}
	password, ok := parsed.User.Password()
	return ok && password == "***"
}

// Credential shadows do not own tickets. Keep their existing forwarding policy
// instead of imposing a gate for a key the harvester never populates.
func isOpenAICodexTicketAccount(account *Account) bool {
	return account != nil && account.IsOpenAIOAuthLike() && !account.IsShadow()
}

// IsOpenAICodexTicketPrivateExtraKey also covers the retired account-level proxy
// override, whose credentials may remain in older account records.
func IsOpenAICodexTicketPrivateExtraKey(key string) bool {
	return IsOpenAICodexTicketExtraKey(key) || key == "codex_harvest_proxy_url"
}

// RedactOpenAICodexTicketExtra strips ephemeral ticket material from exports
// without changing the source account or unrelated backup fields.
func RedactOpenAICodexTicketExtra(extra map[string]any) map[string]any {
	redacted := maps.Clone(extra)
	for key := range redacted {
		if IsOpenAICodexTicketPrivateExtraKey(key) {
			delete(redacted, key)
		}
	}
	return redacted
}
