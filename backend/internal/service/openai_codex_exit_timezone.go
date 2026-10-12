package service

import (
	"bytes"
	"regexp"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// 账号 extra 键：codex_exit_timezone（IANA 名，如 Asia/Manila / Europe/London）。
// 语义：该 OpenAI OAuth 账号的代理出口所在地时区。空/缺省 = 不改写（关闭）。
const codexExitTimezoneExtraKey = "codex_exit_timezone"

// codexExitTimezoneSourceValues 允许改写的客户端时区源值白名单。
// 三者（Asia/Hong_Kong、Asia/Shanghai 与池主出口 Asia/Manila）同为 UTC+8
// 且无夏令时——标签替换是纯地理对齐，模型对「今天几号/现在几点」的判断
// 完全一致（2026-10-13 operator 拍板只做这一档）。America/Los_Angeles 等
// 大偏移值不在其列：改成出口时区会真实污染模型的时间判断。
var codexExitTimezoneSourceValues = []string{
	"Asia/Hong_Kong",
	"Asia/Shanghai",
}

// Go encoding/json 默认把 < > & 转义为 < > &（HTML 安全）：请求体
// 一经 Unmarshal/Marshal 重序列化，environment_context 标签就是转义形态。
// 两种形态都要认（2026-10-13 首版只认原始形态，生产 body 已被重序列化，
// 一次没命中——TZ-REWRITE-ENTER 有、HIT 无，靠观测日志抓到）。
var codexTimezoneTagForms = [][2][]byte{
	{[]byte("<timezone>"), []byte("</timezone>")},
	{[]byte(`\u003ctimezone\u003e`), []byte(`\u003c/timezone\u003e`)},
}

// IANA 时区名形态校验：值会出现在 <timezone> XML 标签里，必须防注入。
var codexExitTimezoneNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+\-]*(/[A-Za-z0-9_+\-]+){1,2}$`)

// codexExitTimezoneFromExtra 读取账号声明的出口时区；空串或非法形态一律
// 返回空串（宁可不改写，也不把畸形值写进请求体）。
func codexExitTimezoneFromExtra(extra map[string]any) string {
	if extra == nil {
		return ""
	}
	raw, _ := extra[codexExitTimezoneExtraKey].(string)
	tz := strings.TrimSpace(raw)
	if tz == "" || len(tz) > 64 || !codexExitTimezoneNamePattern.MatchString(tz) {
		return ""
	}
	return tz
}

// rewriteCodexExitTimezoneForAccount 把请求体 environment_context 块里的客户端
// 真实时区标签改写为该账号的出口时区。返回 (新 body, 是否改写)。
//
// 背景：多人共享的 OAuth 账号经代理出口（菲律宾/伦敦）访问 OpenAI，而 Codex
// 客户端在 environment_context 里携带用户真实时区（Asia/Hong_Kong 等），
// 出口 IP 与请求体时区互相矛盾，上游可见。
//
// 防线（缺一不改）：
//   - 只对 OpenAI OAuth 类账号生效（API key 直连接口没有共享账号语义）；
//   - 账号 extra 显式配置 codex_exit_timezone（缺省关闭，fail-open）；
//   - 只改写白名单源值（与 Manila 同 UTC 偏移的两个）；
//   - body 须含 <cwd>（environment_context 块指纹），避免误改用户消息里
//     引用的同名字面量。
//
// 配置纪律：只给出口在 UTC+8 的账号（菲律宾代理）配 Asia/Manila。伦敦出口的
// 账号不要配——UTC+8 → Europe/London 差 7-8 小时，会真实污染模型时间判断。
// 原始字节级替换：JSON 字符串里 < > / 不需转义，字面量在 raw body 中逐字节
// 出现；全量 ReplaceAll 正常（environment_context 块每请求只出现一次）。
func rewriteCodexExitTimezoneForAccount(account *Account, body []byte) ([]byte, bool) {
	if account == nil || !account.IsOpenAIOAuthLike() || len(body) == 0 {
		return body, false
	}
	tz := codexExitTimezoneFromExtra(account.Extra)
	if tz == "" {
		return body, false
	}
	// environment_context 块指纹（双形态：<cwd> 与其转义形态 <cwd>）。
	hasEnvBlock := bytes.Contains(body, []byte("<cwd>")) || bytes.Contains(body, []byte(`\u003ccwd\u003e`))
	if !hasEnvBlock {
		return body, false
	}
	changed := false
	for _, from := range codexExitTimezoneSourceValues {
		for _, tag := range codexTimezoneTagForms {
			needle := append(append([]byte{}, tag[0]...), append([]byte(from), tag[1]...)...)
			if !bytes.Contains(body, needle) {
				continue
			}
			// 同形态写回，不与 body 既有编码形态混排。
			repl := append(append([]byte{}, tag[0]...), append([]byte(tz), tag[1]...)...)
			body = bytes.ReplaceAll(body, needle, repl)
			changed = true
		}
	}
	if changed {
		logger.LegacyPrintf("service.openai_gateway", "[TZ-REWRITE-HIT] account=%d exit_tz=%s", account.ID, tz)
	}
	return body, changed
}
