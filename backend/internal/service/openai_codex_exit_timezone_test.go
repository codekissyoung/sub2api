package service

import (
	"strings"
	"testing"
)

func tzAccount(extra map[string]any, typ string) *Account {
	return &Account{Platform: PlatformOpenAI, Type: typ, Extra: extra}
}

func TestRewriteCodexExitTimezoneForAccount(t *testing.T) {
	body := `{"model":"gpt-6.1-sol","input":[{"type":"message","content":[{"type":"input_text","text":"<environment_context>\n<cwd>/home/u/proj</cwd>\n<shell>bash</shell>\n<timezone>Asia/Hong_Kong</timezone>\n</environment_context>"}]}]}`

	t.Run("rewrites Hong_Kong to configured exit timezone", func(t *testing.T) {
		acct := tzAccount(map[string]any{"codex_exit_timezone": "Asia/Manila"}, AccountTypeOAuth)
		next, changed := rewriteCodexExitTimezoneForAccount(acct, []byte(body))
		if !changed {
			t.Fatal("expected change")
		}
		if !strings.Contains(string(next), "<timezone>Asia/Manila</timezone>") {
			t.Fatalf("target timezone missing: %s", next)
		}
		if strings.Contains(string(next), "Asia/Hong_Kong") {
			t.Fatalf("source timezone still present: %s", next)
		}
	})

	t.Run("rewrites Shanghai too", func(t *testing.T) {
		sh := strings.Replace(body, "Asia/Hong_Kong", "Asia/Shanghai", 1)
		acct := tzAccount(map[string]any{"codex_exit_timezone": "Asia/Manila"}, AccountTypeOAuth)
		next, changed := rewriteCodexExitTimezoneForAccount(acct, []byte(sh))
		if !changed || !strings.Contains(string(next), "<timezone>Asia/Manila</timezone>") {
			t.Fatalf("Shanghai source not rewritten: changed=%v", changed)
		}
	})

	t.Run("London exit value is honored as configured", func(t *testing.T) {
		acct := tzAccount(map[string]any{"codex_exit_timezone": "Europe/London"}, AccountTypeOAuth)
		next, changed := rewriteCodexExitTimezoneForAccount(acct, []byte(body))
		if !changed || !strings.Contains(string(next), "<timezone>Europe/London</timezone>") {
			t.Fatalf("configured exit value not applied: changed=%v", changed)
		}
	})

	t.Run("non-whitelisted source timezone untouched", func(t *testing.T) {
		la := strings.Replace(body, "Asia/Hong_Kong", "America/Los_Angeles", 1)
		acct := tzAccount(map[string]any{"codex_exit_timezone": "Asia/Manila"}, AccountTypeOAuth)
		next, changed := rewriteCodexExitTimezoneForAccount(acct, []byte(la))
		if changed {
			t.Fatalf("LA source must not be rewritten (large offset would corrupt model date sense): %s", next)
		}
	})

	t.Run("no extra key means off", func(t *testing.T) {
		acct := tzAccount(nil, AccountTypeOAuth)
		if _, changed := rewriteCodexExitTimezoneForAccount(acct, []byte(body)); changed {
			t.Fatal("unset extra must be off")
		}
		acct2 := tzAccount(map[string]any{"codex_exit_timezone": "  "}, AccountTypeOAuth)
		if _, changed := rewriteCodexExitTimezoneForAccount(acct2, []byte(body)); changed {
			t.Fatal("blank extra must be off")
		}
	})

	t.Run("invalid timezone value rejected (injection guard)", func(t *testing.T) {
		for _, bad := range []string{"Asia/Manila</timezone><script>", "Asia/Manila x", "../etc", "a/b/c/d"} {
			acct := tzAccount(map[string]any{"codex_exit_timezone": bad}, AccountTypeOAuth)
			if _, changed := rewriteCodexExitTimezoneForAccount(acct, []byte(body)); changed {
				t.Fatalf("invalid timezone %q must be rejected", bad)
			}
		}
	})

	t.Run("api-key accounts untouched", func(t *testing.T) {
		acct := tzAccount(map[string]any{"codex_exit_timezone": "Asia/Manila"}, AccountTypeAPIKey)
		if _, changed := rewriteCodexExitTimezoneForAccount(acct, []byte(body)); changed {
			t.Fatal("API key account must not be rewritten")
		}
	})

	t.Run("body without environment_context fingerprint untouched", func(t *testing.T) {
		// 用户消息里引用了同名字面量，但 body 没有 <cwd>（无 environment_context 块）
		userOnly := `{"model":"gpt-6.1-sol","input":[{"type":"message","content":[{"type":"input_text","text":"please explain what <timezone>Asia/Hong_Kong</timezone> means"}]}]}`
		acct := tzAccount(map[string]any{"codex_exit_timezone": "Asia/Manila"}, AccountTypeOAuth)
		if _, changed := rewriteCodexExitTimezoneForAccount(acct, []byte(userOnly)); changed {
			t.Fatal("must not rewrite without environment_context fingerprint")
		}
	})

	t.Run("nil account / empty body safe", func(t *testing.T) {
		if _, changed := rewriteCodexExitTimezoneForAccount(nil, []byte(body)); changed {
			t.Fatal("nil account must be a no-op")
		}
		acct := tzAccount(map[string]any{"codex_exit_timezone": "Asia/Manila"}, AccountTypeOAuth)
		if _, changed := rewriteCodexExitTimezoneForAccount(acct, nil); changed {
			t.Fatal("empty body must be a no-op")
		}
	})
}
