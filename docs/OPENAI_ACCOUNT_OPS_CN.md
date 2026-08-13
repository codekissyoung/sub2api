# OpenAI OAuth 账号运维手册（探测 / 诊断 / 轮换 / 删除）

本文沉淀 2026-08-13 处理 dahunt.x1 / idikrm11 两个死号的实战经验，适用于 Codex 号池（platform=openai, type=oauth）的日常运维。

纪律：**token 不离开生产机，不回显原文**（只回显长度、前缀、JWT claims）；生产库写操作必须先获用户确认。

## 1. 只读诊断（DB）

```bash
ssh ice-do-db 'sudo -n -u postgres psql sub2api -Atc "
  SELECT id, status, error_message, last_used_at, rate_limited_at, rate_limit_reset_at
  FROM accounts WHERE credentials->>'"'"'email'"'"' = '"'"'<EMAIL>'"'"'"'
```

关键字段：

- `status` / `error_message`：`error` + `token_invalidated` = AT 已被上游吊销。
- `credentials->>'expires_at'`：AT 过期时间（JWT exp 一致）。
- `extra`（jsonb）里有配额快照：`codex_5h_used_percent` / `codex_7d_used_percent`（主限制看 `codex_primary_used_percent`）、`codex_7d_reset_at`、`codex_usage_updated_at`（上次配额查询时间——查询成功本身证明 AT 活着）、`import_source`（如 `codex_session` 表示从 Codex 会话导入，凭证链可能被多方持有）。

### 本机解码 AT 的 JWT（token 不出主机）

```bash
ssh ice-do-db 'sudo -n -u postgres psql sub2api -Atc "SELECT credentials->>'"'"'access_token'"'"' FROM accounts WHERE id=<ID>" | python3 -c "
import sys,base64,json,datetime
p=sys.stdin.read().strip().split(\".\")[1]
p+=\"=\"*(-len(p)%4)
d=json.loads(base64.urlsafe_b64decode(p))
for k in (\"exp\",\"iat\"):
    print(k, datetime.datetime.fromtimestamp(d[k],datetime.timezone.utc).isoformat())
"'
```

注意：`exp` 未到 ≠ 有效。服务端吊销（改密/登出所有设备/风控）后 AT 立刻死，`expires_at` 无意义。

## 2. AT 实测探测（推理端点）

直接对 Codex 上游发一个最小请求，模型用 `gpt-5.4-mini`（便宜，且可验证限流是否按模型区分——实测**不按模型**，是账号级用量池）：

```bash
ssh ice-do-db 'bash -s' <<'REMOTE'
AT=$(sudo -n -u postgres psql sub2api -Atc "SELECT credentials->>'access_token' FROM accounts WHERE id=<ID>")
ACCT=$(sudo -n -u postgres psql sub2api -Atc "SELECT credentials->>'chatgpt_account_id' FROM accounts WHERE id=<ID>")
CODE=$(curl -sS --max-time 90 -o /tmp/probe.body -D /tmp/probe.hdr -w '%{http_code}' \
  -X POST https://chatgpt.com/backend-api/codex/responses \
  -H "Authorization: Bearer $AT" -H "Content-Type: application/json" \
  -H "Accept: text/event-stream" -H "OpenAI-Beta: responses=experimental" \
  -H "originator: codex-tui" -H "User-Agent: codex-tui/0.141.0" \
  -H "chatgpt-account-id: $ACCT" \
  -d '{"model":"gpt-5.4-mini","instructions":"You are a helpful assistant.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"reply with ok"}]}],"stream":true,"store":false}')
echo "HTTP_STATUS=$CODE"; head -c 600 /tmp/probe.body; rm -f /tmp/probe.body /tmp/probe.hdr
REMOTE
```

结果判读：

| 结果 | 含义 |
|---|---|
| 200 SSE | AT 活着且有余量 |
| 429 `usage_limit_reached` | AT 活着，账号级限流；`resets_at`（epoch）是**上游权威重置时间**，以它为准，不要信 DB 里旧的 `rate_limit_reset_at` |
| 401 `token_invalidated` | AT 被吊销（未过期也会死），号只能靠重新授权救 |
| Cloudflare 质询页 | 服务端用的是 Chrome TLS 指纹客户端（`req_client_pool`），裸 curl 偶尔会被 CF 拦，换 app 内「测试账号」路径再试 |

配额只读端点（app 的「查询额度」走这里）：`GET https://chatgpt.com/backend-api/wham/usage`。

## 3. RT 语义（最重要的心智模型）

- OpenAI 的 RT 是**一次性轮换**的：每用 RT 刷一次，上游发新 RT、**旧 RT 立即作废**。谁手里有链谁后刷谁是活的。
- 因此「刷新令牌」和「轮换 RT」是两件事（bff3fe236 起后台菜单已拆开，均有确认框）：
  - 刷新 = 用 RT 换新 AT（顺带被迫轮换 RT 并落库）；
  - 轮换 RT = 显式换整条链，**不可逆**；若上游已消费旧 RT 但新凭证保存失败，只能重新授权。
- 上游错误码判读：
  - `invalid_refresh_token`：这条 RT 无效——典型是**链在别处被消费**（号商共享凭证/别处也登录了同一账号）。
  - `refresh_token_invalidated`（"Your session has ended"）：**整个 session 被端**（改密/登出所有设备/风控），AT+RT 一起死。
- 没有账号密码 = 无法重新授权 = RT 死了号就是耗材。买号尽量要到可自助登录的方式；只给 RT 的号按一次性耗材对待。

## 4. 删除账号与留痕

后台删除是**软删**（ent SoftDeleteMixin → `UPDATE accounts SET deleted_at`），usage 历史保留。直改库时镜像 `accountRepository.Delete` 的事务：

```sql
BEGIN;
DELETE FROM account_groups WHERE account_id = <ID>;
DELETE FROM scheduled_test_plans WHERE account_id = <ID>;
UPDATE accounts SET deleted_at = NOW(), updated_at = NOW() WHERE id = <ID> AND deleted_at IS NULL;
COMMIT;
```

注意：

- 直改库**没有面板审计日志**，必须另行留痕：追加到生产机 `/home/iec/deploy/release-ledger/account-ops.jsonl`（JSONL，含 ts/action/account_id/email/原因/证据/方式）。
- error 状态的死号不在调度快照里，删除对流量零影响；调度器周期性全量重建会自行收敛。
- 软删可恢复（清 `deleted_at` 即可），但 token 已死的号恢复无意义。
