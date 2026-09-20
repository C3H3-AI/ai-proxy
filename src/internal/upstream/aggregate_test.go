package upstream

import (
	"strings"
	"testing"
)

// TestAggregateEmptyContentDoesNotLatch —— 回归测试：上游 issue #142 同款。
//
// 背景：Aggregate 里 gotAnyContent 既表示「已取到正文」，又用作
// 「是否还要读 message 形态正文」的开关（`&& !gotAnyContent`）。
// 原实现只要收到 content 字段（**即使是空串**）就置 true，
// 于是首帧空 content 会把后面 message 形态的正文整段吞掉。
//
// 修复：空串不置 latch，且所有正文写入都走同一个 appendContent。
func TestAggregateEmptyContentDoesNotLatch(t *testing.T) {
	// 第一帧：空 content（常见于 role 帧）；第二帧：完整 message 形态正文
	sse := strings.Join([]string{
		`data: {"id":"c1","model":"m","choices":[{"delta":{"role":"assistant","content":""}}]}`,
		``,
		`data: {"id":"c1","model":"m","choices":[{"message":{"role":"assistant","content":"HELLO_FROM_MESSAGE"}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	out, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	msg := extractMessage(t, out)
	if got := msg["content"]; got != "HELLO_FROM_MESSAGE" {
		t.Fatalf("content=%q，正文被空 content 帧屏蔽（#142 回归）", got)
	}
}

// TestAggregateNormalContentStillWorks —— 确保修复没破坏常规累积。
func TestAggregateNormalContentStillWorks(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"c1","choices":[{"delta":{"role":"assistant","content":"He"}}]}`,
		``,
		`data: {"id":"c1","choices":[{"delta":{"content":"llo"}}]}`,
		``,
		`data: {"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"total_tokens":7}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	out, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	msg := extractMessage(t, out)
	if got := msg["content"]; got != "Hello" {
		t.Errorf("content=%q want Hello", got)
	}
	if _, ok := out["usage"]; !ok {
		t.Errorf("usage 应被保留")
	}
	if fr := finishReasonOf(t, out); fr != "stop" {
		t.Errorf("finish_reason=%q want stop", fr)
	}
}

// TestAggregateToolCallsMissingIndexNotCollapsed —— 回归测试。
//
// 背景：原实现把「缺 index」的 tool_call 一律当作 index 0，
// 于是多个 tool_call 挤进同一条目互相覆盖 —— 表现为
// tool_call 数量变少、arguments 拼接错乱（Codex CLI 第二轮报
// tool_call_sequence_broken 的根因之一）。
//
// 修复：index 分派顺序 = 显式 index → id 复用/新分配 →
// 沿用上一个 index（流式分片语义）→ 跳号新分配。
func TestAggregateToolCallsMissingIndexNotCollapsed(t *testing.T) {
	// 三个 tool_call，全部缺 index，各自带不同 id
	sse := strings.Join([]string{
		`data: {"id":"c1","choices":[{"delta":{"tool_calls":[{"id":"call_a","type":"function","function":{"name":"f_a","arguments":"{\"x\":1}"}}]}}]}`,
		``,
		`data: {"id":"c1","choices":[{"delta":{"tool_calls":[{"id":"call_b","type":"function","function":{"name":"f_b","arguments":"{\"y\":2}"}}]}}]}`,
		``,
		`data: {"id":"c1","choices":[{"delta":{"tool_calls":[{"id":"call_c","type":"function","function":{"name":"f_c","arguments":"{\"z\":3}"}}]}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	out, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	msg := extractMessage(t, out)
	calls, ok := msg["tool_calls"].([]map[string]any)
	if !ok {
		t.Fatalf("tool_calls 类型=%T", msg["tool_calls"])
	}
	if len(calls) != 3 {
		t.Fatalf("tool_calls 数量=%d want 3（缺 index 被压缩到同一条目）", len(calls))
	}
	wantIDs := map[string]bool{"call_a": true, "call_b": true, "call_c": true}
	for _, c := range calls {
		id, _ := c["id"].(string)
		if !wantIDs[id] {
			t.Errorf("意外的 tool_call id=%q", id)
		}
		delete(wantIDs, id)
	}
	if len(wantIDs) != 0 {
		t.Errorf("丢失 tool_call: %v", wantIDs)
	}
}

// TestAggregateToolCallChunkedByIdReuse —— 同一 id 的分片必须合并（不得新建条目）。
func TestAggregateToolCallChunkedByIdReuse(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"c1","choices":[{"delta":{"tool_calls":[{"id":"call_x","type":"function","function":{"name":"f","arguments":"{\"a\":"}}]}}]}`,
		``,
		`data: {"id":"c1","choices":[{"delta":{"tool_calls":[{"id":"call_x","function":{"arguments":"1}"}}]}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	out, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	calls, _ := extractMessage(t, out)["tool_calls"].([]map[string]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls 数量=%d want 1（同 id 分片应合并）", len(calls))
	}
	fn, _ := calls[0]["function"].(map[string]any)
	if got, _ := fn["arguments"].(string); got != `{"a":1}` {
		t.Errorf("arguments=%q want {\"a\":1}", got)
	}
}

// TestAggregateToolCallExplicitIndexStillWins —— 显式 index 必须优先于推断。
func TestAggregateToolCallExplicitIndexStillWins(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"c1","choices":[{"delta":{"tool_calls":[{"index":5,"id":"call_z","type":"function","function":{"name":"fz","arguments":"{}"}}]}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	out, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	calls, _ := extractMessage(t, out)["tool_calls"].([]map[string]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls 数量=%d want 1", len(calls))
	}
	if idx, _ := calls[0]["index"].(float64); int(idx) != 5 {
		// index 可能以 int 存储
		if i2, ok := calls[0]["index"].(int); !ok || i2 != 5 {
			t.Errorf("index=%v want 5", calls[0]["index"])
		}
	}
}

// ── 测试辅助 ──────────────────────────────────────────────────────

func extractMessage(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	choices, ok := out["choices"].([]any)
	if !ok || len(choices) == 0 {
		t.Fatalf("choices 缺失或为空: %#v", out["choices"])
	}
	c0, _ := choices[0].(map[string]any)
	msg, ok := c0["message"].(map[string]any)
	if !ok {
		t.Fatalf("message 缺失: %#v", c0)
	}
	return msg
}

func finishReasonOf(t *testing.T, out map[string]any) string {
	t.Helper()
	choices, _ := out["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	c0, _ := choices[0].(map[string]any)
	fr, _ := c0["finish_reason"].(string)
	return fr
}
