package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestPrepareBodySanitizesFingerprint —— 端到端：发往上游前的 PrepareBody
// 必须已完成指纹脱敏（否则 Claude Code / Codex 请求会被上游 400 code=11128 拦截）。
func TestPrepareBodySanitizesFingerprint(t *testing.T) {
	const raw = "You are Claude Code, Anthropic's official CLI for Claude."
	src, _ := json.Marshal(map[string]any{
		"model":    "workbuddy/x",
		"messages": []any{map[string]any{"role": "system", "content": raw}},
	})
	out := PrepareBody(src)

	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs, _ := obj["messages"].([]any)
	got, _ := msgs[0].(map[string]any)["content"].(string)
	if strings.Contains(got, "official CLI for Claude") {
		t.Errorf("PrepareBody 未脱敏，指纹仍逐字存在：%q", got)
	}
	// 同时确认 stream:true 仍在（脱敏不应破坏既有改写）
	if obj["stream"] != true {
		t.Errorf("stream 应仍为 true，实际=%v", obj["stream"])
	}
}
