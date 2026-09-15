package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStreamRejectsNonSSEErrorIn200 —— H3 核心回归测试。
//
// 上游并非只在 status>=400 时报错：同包 doJSONWith / FetchModels /
// FetchModelPricing 三处都显式处理 `code != 0` 的 200 响应，
// 说明「200 + 错误 body」是真实形态。
//
// 此前 Stream 只做逐行透传，会把这类非 SSE 内容原样写给客户端：
//   - OpenAI SDK 解析失败或静默忽略；
//   - 账号不会被 Classify 判定 → 不冷却 → 后续继续选中它。
func TestStreamRejectsNonSSEErrorIn200(t *testing.T) {
	cases := []struct {
		name string
		body string
		want ErrKind
	}{
		{"余额不足信封", `{"code":1,"msg":"余额不足"}` + "\n", ErrHardCredit},
		{"积分不足关键词", `{"code":10001,"msg":"积分不足，请充值"}` + "\n", ErrHardCredit},
		{"session 失效", `{"code":12153,"msg":"Offline user session not found"}` + "\n", ErrSessionDead},
		{"未知 code", `{"code":9999,"msg":"weird"}` + "\n", ErrClient},
		{"非 JSON 裸文本", "upstream exploded\n", ErrClient},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			err := Stream(rec, strings.NewReader(c.body))
			if err == nil {
				t.Fatalf("期望返回错误，实际 nil；响应体=%q（说明非 SSE 内容被透传了）", rec.Body.String())
			}
			ue, ok := err.(*Error)
			if !ok {
				t.Fatalf("期望 *Error，得到 %T: %v", err, err)
			}
			if ue.Kind != c.want {
				t.Errorf("Kind = %v, 期望 %v", ue.Kind, c.want)
			}
			// 关键：内容不得被写给客户端
			if rec.Body.Len() > 0 {
				t.Errorf("非 SSE 内容被透传给客户端: %q", rec.Body.String())
			}
		})
	}
}

// TestStreamPassesThroughNormalSSE —— 确保嗅探不误伤正常流。
func TestStreamPassesThroughNormalSSE(t *testing.T) {
	body := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(body)); err != nil {
		t.Fatalf("正常流不应报错: %v", err)
	}
	if got := rec.Body.String(); got != body {
		t.Errorf("透传内容不一致:\n got=%q\nwant=%q", got, body)
	}
}

// TestStreamKeepsLeadingBlankLinesAndComments —— SSE 允许前导空行与注释行。
func TestStreamKeepsLeadingBlankLinesAndComments(t *testing.T) {
	body := "\n: keepalive comment\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(body)); err != nil {
		t.Fatalf("含注释/空行的流不应报错: %v", err)
	}
	if got := rec.Body.String(); got != body {
		t.Errorf("透传内容不一致:\n got=%q\nwant=%q", got, body)
	}
}

// TestStreamAcceptsEventAndIDFields —— 合法的其它 SSE 字段不得被误判。
func TestStreamAcceptsEventAndIDFields(t *testing.T) {
	body := "event: message\nid: 42\nretry: 1000\ndata: {\"choices\":[]}\n\ndata: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(body)); err != nil {
		t.Fatalf("合法 SSE 字段被误判为错误: %v", err)
	}
	if got := rec.Body.String(); got != body {
		t.Errorf("透传内容不一致:\n got=%q\nwant=%q", got, body)
	}
}

// TestStreamEnsuresDone —— 上游未发 [DONE] 时补齐（既有行为不得回退）。
func TestStreamEnsuresDone(t *testing.T) {
	rec := httptest.NewRecorder()
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"
	if err := Stream(rec, strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Errorf("未补齐 [DONE]: %q", rec.Body.String())
	}
}

// TestStreamHeadersSet —— 正常流应设置 SSE headers。
func TestStreamHeadersSet(t *testing.T) {
	rec := httptest.NewRecorder()
	body := "data: [DONE]\n\n"
	if err := Stream(rec, strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
}

// TestStreamEmptyBody —— 空流不得 panic，应正常收尾。
func TestStreamEmptyBody(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader("")); err != nil {
		t.Fatalf("空流不应报错: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Errorf("空流应收尾为 [DONE]: %q", rec.Body.String())
	}
}

// TestClassifyNonSSELine 直接覆盖判定函数。
func TestClassifyNonSSELine(t *testing.T) {
	notErr := []string{
		"data: {}", "data: [DONE]", "event: x", "id: 1", "retry: 5", ":comment", "",
	}
	for _, s := range notErr {
		if _, _, isErr := classifyNonSSELine(s); isErr {
			t.Errorf("合法 SSE 行被误判为错误: %q", s)
		}
	}
	mustErr := []string{
		`{"code":1,"msg":"余额不足"}`, "garbage", `{"code":0,"data":{}}`,
	}
	for _, s := range mustErr {
		if _, _, isErr := classifyNonSSELine(s); !isErr {
			t.Errorf("非 SSE 行未被判为错误: %q", s)
		}
	}
}

var _ = http.StatusOK
