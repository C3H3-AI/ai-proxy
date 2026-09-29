package server

import (
	"encoding/json"
	"testing"
)

// ── 上游 wild-work 1b38ac1 / PR #51 同款 ──────────────────────────

// 用真实 JSON 走一遍 rewriteModel，再解析回来检查 content。
func rewriteAndDecode(t *testing.T, body string, model string) []any {
	t.Helper()
	out, err := rewriteModel([]byte(body), model)
	if err != nil {
		t.Fatalf("rewriteModel: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("结果非 JSON: %v", out)
	}
	if obj["model"] != model {
		t.Errorf("model=%v want %v", obj["model"], model)
	}
	msgs, _ := obj["messages"].([]any)
	return msgs
}

// TestNormalizeToolTurnContentNull —— 核心回归。
//
// 带 tool_calls 的 assistant 消息 content 为 null 时，上游会**整请求拒答**，
// 且给一个误导性的 tool 配对错误。这里必须补成空串。
func TestNormalizeToolTurnContentNull(t *testing.T) {
	body := `{"model":"x","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":null}
	]}`
	msgs := rewriteAndDecode(t, body, "workbuddy/glm-5.2")
	if len(msgs) != 3 {
		t.Fatalf("消息数=%d want 3", len(msgs))
	}
	asst, _ := msgs[1].(map[string]any)
	if got, ok := asst["content"].(string); !ok || got != "" {
		t.Errorf("assistant content=%#v want \"\" (string)", asst["content"])
	}
	tool, _ := msgs[2].(map[string]any)
	if got, ok := tool["content"].(string); !ok || got != "" {
		t.Errorf("tool content=%#v want \"\" (string)", tool["content"])
	}
}

// TestNormalizeToolTurnContentMissing —— content 字段缺失同样要补。
func TestNormalizeToolTurnContentMissing(t *testing.T) {
	body := `{"model":"x","messages":[
		{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}
	]}`
	msgs := rewriteAndDecode(t, body, "workbuddy/glm-5.2")
	asst, _ := msgs[0].(map[string]any)
	if got, ok := asst["content"].(string); !ok || got != "" {
		t.Errorf("缺失 content 应补成 \"\"，实际 %#v", asst["content"])
	}
}

// TestNormalizeToolTurnContentKeepsNormalContent —— 不得误伤正常正文。
//
// 有正文的 assistant 消息必须原样保留，不能被清成空串。
func TestNormalizeToolTurnContentKeepsNormalContent(t *testing.T) {
	body := `{"model":"x","messages":[
		{"role":"assistant","content":"我要调用工具","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"结果"}
	]}`
	msgs := rewriteAndDecode(t, body, "workbuddy/glm-5.2")
	asst, _ := msgs[0].(map[string]any)
	if got := asst["content"]; got != "我要调用工具" {
		t.Errorf("正常正文被改写: %#v", got)
	}
	tool, _ := msgs[1].(map[string]any)
	if got := tool["content"]; got != "结果" {
		t.Errorf("tool 正常内容被改写: %#v", got)
	}
}

// TestNormalizeToolTurnContentLeavesPlainAssistant —— 普通 assistant 消息
// （不带 tool_calls）的 content 不被擅自改写 —— 无证据表明它有问题。
func TestNormalizeToolTurnContentLeavesPlainAssistant(t *testing.T) {
	body := `{"model":"x","messages":[
		{"role":"assistant","content":null},
		{"role":"user","content":"ok"}
	]}`
	msgs := rewriteAndDecode(t, body, "workbuddy/glm-5.2")
	asst, _ := msgs[0].(map[string]any)
	if _, exists := asst["content"]; !exists {
		t.Fatal("content 字段不应被删除")
	}
	if asst["content"] != nil {
		t.Errorf("不带 tool_calls 的 assistant content 应保持原样，实际 %#v", asst["content"])
	}
}

// TestNormalizeToolTurnContentNonString —— 非字符串 content（如数字）
// 也要规范成字符串，否则上游同样会拒。
func TestNormalizeToolTurnContentNonString(t *testing.T) {
	body := `{"model":"x","messages":[
		{"role":"assistant","content":123,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}
	]}`
	msgs := rewriteAndDecode(t, body, "workbuddy/glm-5.2")
	asst, _ := msgs[0].(map[string]any)
	if got, ok := asst["content"].(string); !ok || got != "" {
		t.Errorf("非字符串 content 应规范为 \"\"，实际 %#v", asst["content"])
	}
}
