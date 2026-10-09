package sanitize

import (
	"encoding/json"
	"strings"
	"testing"
)

// 用于测试的 Claude Code / Codex 指纹常量（与上游回归用例同源）。
const (
	ccIdentity = "You are Claude Code, Anthropic's official CLI for Claude."
	ccBranch   = "Main branch (you will usually use this for PRs)"
	codexIntro = "You are a coding agent running in the Codex CLI, a terminal-based coding assistant."
	ccFeedback = "To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues"
)

// bodyWith 构造带指定 system 提示的请求体。
func bodyWith(t *testing.T, sys string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"model": "workbuddy/test",
		"messages": []any{
			map[string]any{"role": "system", "content": sys},
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestMessagesRewritesFingerprint —— 核心：Claude Code 身份句被最小改写，
// 语义不变但不再逐字命中上游审核。
func TestMessagesRewritesFingerprint(t *testing.T) {
	out := Messages(bodyWith(t, ccIdentity))
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs, _ := obj["messages"].([]any)
	sys, _ := msgs[0].(map[string]any)["content"].(string)

	if strings.Contains(sys, "official CLI for Claude") {
		t.Errorf("原始指纹句仍逐字存在，会被上游审核拦截：%q", sys)
	}
	if !strings.Contains(sys, "Claude Code") {
		t.Errorf("改写后应保留「Claude Code」语义，实际=%q", sys)
	}
	if !strings.Contains(sys, "CLI tool") {
		t.Errorf("应改写为「CLI tool」，实际=%q", sys)
	}
}

// TestMessagesMultipleFingerprints —— 多段指纹（分支说明、Codex 自述、反馈指引）都被改写。
func TestMessagesMultipleFingerprints(t *testing.T) {
	sys := ccIdentity + "\n" + ccBranch + "\n" + codexIntro + "\n" + ccFeedback
	out := Messages(bodyWith(t, sys))
	s := string(out)

	for _, raw := range []string{
		"official CLI for Claude",
		"Main branch (you will usually use this for PRs)",
		"running in the Codex CLI,",
		"To give feedback,",
	} {
		if strings.Contains(s, raw) {
			t.Errorf("指纹 %q 未被改写，仍可能被上游拦截", raw)
		}
	}
	// 语义保留
	for _, keep := range []string{"Claude Code", "Default branch", "Codex CLI", "claude-code/issues"} {
		if !strings.Contains(s, keep) {
			t.Errorf("语义片段 %q 不应丢失", keep)
		}
	}
}

// TestMessagesStripsBillingHeader —— 键值型指纹应整段删除。
func TestMessagesStripsBillingHeader(t *testing.T) {
	sys := "prefix x-anthropic-billing-header: cc_entrypoint=abc; scope=xyz; suffix cc_version=1.2;"
	out := Messages(bodyWith(t, sys))
	var obj map[string]any
	json.Unmarshal(out, &obj)
	msgs, _ := obj["messages"].([]any)
	got, _ := msgs[0].(map[string]any)["content"].(string)

	if strings.Contains(got, "cc_entrypoint=") || strings.Contains(got, "cc_version=") {
		t.Errorf("cc_* 键值指纹应被删除，实际=%q", got)
	}
	if !strings.Contains(got, "prefix") || !strings.Contains(got, "suffix") {
		t.Errorf("非指纹正文应保留，实际=%q", got)
	}
}

// TestMessagesBare11128 —— 裸 11128 会被整单拦截，应插入连字符保留可读性。
func TestMessagesBare11128(t *testing.T) {
	out := Messages(bodyWith(t, "error code 11128 happened"))
	s := string(out)
	if strings.Contains(s, "11128") {
		t.Errorf("裸 11128 应被改写（否则整单被拦截），实际=%s", s)
	}
	if !strings.Contains(s, "11-128") {
		t.Errorf("应改写为 11-128 保留可读性，实际=%s", s)
	}
}

// TestMessagesPlainBodyUnchanged —— 无指纹时**不得**改动（零副作用）。
func TestMessagesPlainBodyUnchanged(t *testing.T) {
	src := bodyWith(t, "You are a helpful assistant.")
	out := Messages(src)
	if string(out) != string(src) {
		t.Errorf("无指纹时不应改写：\n src=%s\n out=%s", src, out)
	}
}

// TestMessagesDeepContent —— 多段 content（数组形态）与 tool_calls.arguments 也要净化。
func TestMessagesDeepContent(t *testing.T) {
	b, _ := json.Marshal(map[string]any{
		"messages": []any{
			map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "text", "text": ccIdentity}},
			},
			map[string]any{
				"role":      "assistant",
				"content":   nil,
				"tool_calls": []any{map[string]any{"function": map[string]any{"name": "f", "arguments": ccBranch}}},
			},
		},
	})
	out := Messages(b)
	s := string(out)

	if strings.Contains(s, "official CLI for Claude") {
		t.Error("数组形态 content 未净化")
	}
	if strings.Contains(s, "Main branch (you will usually use this for PRs)") {
		t.Error("tool_calls.arguments 未净化（content 为 null 的 assistant 消息不应跳过）")
	}
}

// TestMessagesInvalidJSONSafe —— 不可解析时原样返回，绝不阻塞请求。
func TestMessagesInvalidJSONSafe(t *testing.T) {
	bad := []byte("{not json")
	if got := Messages(bad); string(got) != string(bad) {
		t.Errorf("不可解析时应原样返回，实际=%q", got)
	}
	if got := Messages(nil); got != nil {
		t.Errorf("nil 应返回 nil，实际=%q", got)
	}
	if got := Messages([]byte{}); len(got) != 0 {
		t.Errorf("空 body 应返回空，实际=%q", got)
	}
}

// TestMessagesPreservesOtherFields —— 只动消息内容，其他字段不变。
func TestMessagesPreservesOtherFields(t *testing.T) {
	b, _ := json.Marshal(map[string]any{
		"model":       "workbuddy/x",
		"temperature": 0.7,
		"messages":    []any{map[string]any{"role": "system", "content": ccIdentity}},
	})
	out := Messages(b)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if obj["model"] != "workbuddy/x" {
		t.Errorf("model 不应被改，实际=%v", obj["model"])
	}
	if obj["temperature"] != 0.7 {
		t.Errorf("temperature 不应被改，实际=%v", obj["temperature"])
	}
}

// TestHasFingerprintQuickPath —— 预检：无指纹应走零分配快速路径。
func TestHasFingerprintQuickPath(t *testing.T) {
	if hasFingerprint("plain text") {
		t.Error("普通文本不应命中指纹")
	}
	for _, s := range []string{ccIdentity, ccBranch, codexIntro, "x-anthropic-billing-header", "11128", "github.com/anthropics/"} {
		if !hasFingerprint(s) {
			t.Errorf("应命中指纹：%q", s)
		}
	}
}
