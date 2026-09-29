package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rockswang/workbuddy-wild/internal/auth"
)

// TestChatRequestBodyTooLarge —— 回归：请求体超限必须明确回 413（issue #30 同款）。
//
// 修复前用 io.LimitReader(r.Body, 8<<20) **静默截断**：
// 超过 8MiB 的请求被砍成非法 JSON，解析失败后误报成 invalid_model，
// 把"请求太大"误导成"模型名格式不对"，排查时极难定位。
func TestChatRequestBodyTooLarge(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})

	// 构造超过 8MiB 的合法 JSON 请求体
	pad := strings.Repeat("x", MaxRequestBody) // 单字段就超过上限
	big := `{"model":"workbuddy/glm-5.2","messages":[{"role":"user","content":"` + pad + `"}]}`

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(big))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限应回 413，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "request_too_large") {
		t.Errorf("错误码应为 request_too_large，实际 body=%s", rec.Body.String())
	}
}

// TestChatRequestBodyAtLimitPasses —— 恰好等于上限的请求必须正常通过
// （不能把边界值本身也拒掉）。
func TestChatRequestBodyAtLimitPasses(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})

	// 目标：总长恰好 = MaxRequestBody
	body := `{"model":"workbuddy/glm-5.2","messages":[{"role":"user","content":"`
	suffix := `"}]}`
	padLen := MaxRequestBody - len(body) - len(suffix)
	if padLen < 0 {
		t.Skip("上限太小，无法构造边界用例")
	}
	exact := body + strings.Repeat("x", padLen) + suffix
	if len(exact) != MaxRequestBody {
		t.Fatalf("构造的边界长度=%d want %d", len(exact), MaxRequestBody)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(exact))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("恰好等于上限不应被拒（实际 %d）", rec.Code)
	}
}

// TestChatMalformedJSONExplicitError —— JSON 非法必须报 invalid_request，
// 不能静默吞掉后拿空 model 去做路由（否则误报 invalid_model）。
func TestChatMalformedJSONExplicitError(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model": "workbuddy/glm-5.2", "messages": [`)) // 截断的 JSON
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 JSON 应回 400，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	bodyStr := rec.Body.String()
	if strings.Contains(bodyStr, "invalid_model") {
		t.Errorf("非法 JSON 不应误报 invalid_model，实际 body=%s", bodyStr)
	}
	if !strings.Contains(bodyStr, "invalid_request") {
		t.Errorf("应报 invalid_request，实际 body=%s", bodyStr)
	}
}

// TestMaxRequestBodyConstant —— 锁定上限常量口径（8MiB，与上游一致）。
func TestMaxRequestBodyConstant(t *testing.T) {
	if MaxRequestBody != 8<<20 {
		t.Errorf("MaxRequestBody=%d want %d (8MiB)", MaxRequestBody, 8<<20)
	}
}
