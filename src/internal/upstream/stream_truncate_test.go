package upstream

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ── 测试辅助 ──────────────────────────────────────────────────────

// errAfterReader 读完给定内容后返回指定错误，模拟「流中途断开」。
type errAfterReader struct {
	data string
	err  error
	done bool
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	n := copy(p, r.data)
	if n >= len(r.data) {
		r.done = true
	}
	return n, nil
}

func streamTo(t *testing.T, rdr *errAfterReader) (string, error) {
	t.Helper()
	rec := httptest.NewRecorder()
	err := Stream(rec, rdr)
	return rec.Body.String(), err
}

// ── 回归测试 ──────────────────────────────────────────────────────

// TestStreamTruncatedDoesNotFakeDone —— 核心回归：上游 issue #42。
//
// 修复前：流中途断开时，出口无条件补 `data: [DONE]`，
// 把半截流伪装成正常结束 → 客户端拿残缺 tool_call arguments 去解析，
// 报 "tool input was not fully received"，而网关日志里什么都看不到。
//
// 修复后：发 `upstream_truncated` error 帧，且**不补 [DONE]**，并返回错误。
func TestStreamTruncatedDoesNotFakeDone(t *testing.T) {
	// 上游发了几帧，然后在中途连接中断
	body := `data: {"id":"c1","choices":[{"delta":{"content":"partial"}}]}

data: {"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1, \"b\":2"}}]}}]}

`
	out, err := streamTo(t, &errAfterReader{
		data: body,
		err:  errors.New("unexpected EOF"),
	})

	if err == nil {
		t.Fatal("截断时必须返回错误（此前静默成功，正是 #42 的问题）")
	}
	if strings.Contains(out, "data: [DONE]") {
		t.Fatalf("截断时不得补 [DONE]（会把半截流伪装成成功）：\n%s", out)
	}
	if !strings.Contains(out, "upstream_truncated") {
		t.Errorf("应发出 upstream_truncated error 帧，实际输出：\n%s", out)
	}
	if !strings.Contains(out, `"type":"upstream_error"`) {
		t.Errorf("error 帧应符合 OpenAI 规范，实际：\n%s", out)
	}
	// 已写出的正常帧必须保留（不能因为报错就丢弃）
	if !strings.Contains(out, `"content":"partial"`) {
		t.Errorf("已透传的正常帧不应丢失：\n%s", out)
	}
}

// TestStreamTruncatedReturnsClassifiedError —— 截断必须带错误分类，
// 否则 handler 的 applyUpstreamError 不会冷却该账号，
// 后续请求会继续选中它（#42 的完整修复）。
func TestStreamTruncatedReturnsClassifiedError(t *testing.T) {
	_, err := streamTo(t, &errAfterReader{
		data: `data: {"id":"c1","choices":[{"delta":{"content":"x"}}]}

`,
		err: errors.New("connection reset by peer"),
	})
	if err == nil {
		t.Fatal("应返回错误")
	}
	ue, ok := err.(*Error)
	if !ok {
		t.Fatalf("应返回 *Error（走 applyUpstreamError 分类），实际 %T: %v", err, err)
	}
	if ue.Kind != ErrServer {
		t.Errorf("Kind=%v want ErrServer（否则不会冷却账号）", ue.Kind)
	}
}

// TestStreamPartialFrameAtEOFIsTruncated —— 最后一行没换行且不是完整 JSON
// （半个帧）→ 必须判为截断，不能补 DONE。
func TestStreamPartialFrameAtEOFIsTruncated(t *testing.T) {
	// 注意：末尾没有 \n，且 JSON 不完整
	body := `data: {"id":"c1","choices":[{"delta":{"content":"ab`

	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(body))
	out := rec.Body.String()

	if err == nil {
		t.Fatal("半个帧应判为截断")
	}
	if strings.Contains(out, "data: [DONE]") {
		t.Errorf("半个帧不得补 [DONE]：\n%s", out)
	}
	if !strings.Contains(out, "upstream_truncated") {
		t.Errorf("应发 upstream_truncated：\n%s", out)
	}
}

// TestStreamNormalEOFMissingDoneStillBackfills —— 正常 EOF 但上游漏发 DONE
// （帧完整、有换行）：必须照旧兜底补，否则误伤全部正常请求。
func TestStreamNormalEOFMissingDoneStillBackfills(t *testing.T) {
	body := `data: {"id":"c1","choices":[{"delta":{"content":"hello"}}]}

`
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(body))
	out := rec.Body.String()

	if err != nil {
		t.Fatalf("正常流不应报错: %v", err)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("上游漏发 DONE 时应兜底补：\n%s", out)
	}
	if strings.Count(out, "data: [DONE]") != 1 {
		t.Errorf("应恰好一个 [DONE]，实际 %d 个", strings.Count(out, "data: [DONE]"))
	}
}

// TestStreamExplicitDoneNotDuplicated —— 上游显式发 DONE：不得重复补。
func TestStreamExplicitDoneNotDuplicated(t *testing.T) {
	body := `data: {"id":"c1","choices":[{"delta":{"content":"hi"}}]}

data: [DONE]

`
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(body))
	out := rec.Body.String()

	if err != nil {
		t.Fatalf("正常流不应报错: %v", err)
	}
	if strings.Count(out, "data: [DONE]") != 1 {
		t.Errorf("上游已发 DONE 时不得重复补，实际 %d 个：\n%s",
			strings.Count(out, "data: [DONE]"), out)
	}
}

// TestStreamEmptyStreamIsError —— 200 但一个有效帧都没有：
// 补 DONE 让客户端收尾，但必须返回错误以便调用方冷却账号。
func TestStreamEmptyStreamIsError(t *testing.T) {
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(""))
	out := rec.Body.String()

	if err == nil {
		t.Fatal("空流应返回错误（否则账号不会被冷却）")
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("空流仍应补 DONE 让客户端收尾：\n%s", out)
	}
	ue, ok := err.(*Error)
	if !ok {
		t.Fatalf("应返回 *Error，实际 %T", err)
	}
	if ue.Kind != ErrServer {
		t.Errorf("Kind=%v want ErrServer", ue.Kind)
	}
}

// TestStreamNonSSEErrorStillDetected —— 200 + 非 SSE 错误信封仍要被嗅探出来
// （这是既有能力，防止本次改动破坏它）。
func TestStreamNonSSEErrorStillDetected(t *testing.T) {
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(`{"code":1,"msg":"余额不足"}
`))
	if err == nil {
		t.Fatal("200 里的非 SSE 错误应被识别")
	}
	ue, ok := err.(*Error)
	if !ok {
		t.Fatalf("应返回 *Error，实际 %T", err)
	}
	if ue.Kind == ErrNone {
		t.Errorf("应给出明确分类，实际 Kind=ErrNone")
	}
	if ue.Kind != ErrHardCredit {
		t.Errorf("余额不足应判 ErrHardCredit，实际 %v", ue.Kind)
	}
}

// TestStreamFlushPreserved —— 逐行 flush 的行为必须保留
// （流式响应的实时性依赖它）。
func TestStreamFlushPreserved(t *testing.T) {
	body := `data: {"id":"c1","choices":[{"delta":{"content":"a"}}]}

data: {"id":"c1","choices":[{"delta":{"content":"b"}}]}

`
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(body)); err != nil {
		t.Fatalf("正常流不应报错: %v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"content":"a"`) || !strings.Contains(out, `"content":"b"`) {
		t.Errorf("所有帧都应透传：\n%s", out)
	}
	if rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Errorf("Content-Type=%q", rec.Header().Get("Content-Type"))
	}
	_ = http.StatusOK
}
