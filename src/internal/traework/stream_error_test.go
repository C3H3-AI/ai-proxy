package traework

import (
	"net/http/httptest"
	"net/http"
	"strings"
	"testing"

	"github.com/rockswang/workbuddy-wild/internal/provider"
)

// streamOf 用给定 SSE 文本跑一次 Stream，返回写出的响应与返回错误。
func streamOf(t *testing.T, sse string) (string, error) {
	t.Helper()
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(sse))
	return rec.Body.String(), err
}

// TestStreamBusinessErrorNotFakedSuccess —— 核心回归：
// 流内业务错误（event:error）必须**不伪装成正常收尾**。
//
// 修复前的行为：把错误描述写进 delta.content 并补 [DONE]，
// 于是  1) 客户端把失败的流当成完整回答；2) handler 收不到错误，账号不被记失败。
//
// 期望：发出 OpenAI 规范 error 帧、**不补 [DONE]**、并返回分类后的错误。
func TestStreamBusinessErrorNotFakedSuccess(t *testing.T) {
	sse := "event:error\n" +
		`data:{"code":1005,"message":"积分不足"}` + "\n\n"
	out, err := streamOf(t, sse)

	if err == nil {
		t.Fatal("流内业务错误应返回 error，供 handler 更新账号状态")
	}
	ue, ok := err.(*provider.Error)
	if !ok {
		t.Fatalf("应返回 *provider.Error，实际 %T: %v", err, err)
	}
	if ue.Kind != provider.ErrHardCredit {
		t.Errorf("1005 应分类为 ErrHardCredit，实际 %v", ue.Kind)
	}
	if strings.Contains(out, "[DONE]") {
		t.Errorf("失败流不应补 [DONE]（否则客户端当正常结束），实际输出：\n%s", out)
	}
	if !strings.Contains(out, `"error"`) {
		t.Errorf("应以 OpenAI 规范 error 帧收尾，实际输出：\n%s", out)
	}
	if !strings.Contains(out, "solo_1005") {
		t.Errorf("error 帧应带上游错误码（solo_1005），实际输出：\n%s", out)
	}
}

// TestStreamRateLimitErrorMapsToSoftRate —— 限流类业务错误应映射为 ErrSoftRate
// （软冷却换号），而不是硬余额冷却。
func TestStreamRateLimitErrorMapsToSoftRate(t *testing.T) {
	for _, code := range []int{3004, 9074} {
		sse := "event:error\n" +
			`data:{"code":` + itoa(code) + `,"message":"rate limited"}` + "\n\n"
		_, err := streamOf(t, sse)
		ue, ok := err.(*provider.Error)
		if !ok {
			t.Fatalf("code=%d 应返回 *provider.Error，实际 %T", code, err)
		}
		if ue.Kind != provider.ErrSoftRate {
			t.Errorf("code=%d 应映射 ErrSoftRate，实际 %v", code, ue.Kind)
		}
	}
}

// TestStreamSuccessUnaffected —— 正常流不受影响：照常补 [DONE]、返回 nil。
func TestStreamSuccessUnaffected(t *testing.T) {
	sse := "event:output\n" +
		`data:{"response":"hello"}` + "\n\n" +
		"event:done\n" +
		`data:{}` + "\n\n"
	out, err := streamOf(t, sse)
	if err != nil {
		t.Fatalf("正常流不应返回 error：%v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("正常流应透传内容，实际输出：\n%s", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Errorf("正常流应补 [DONE]，实际输出：\n%s", out)
	}
}

// TestSOLOStreamErrorKind —— Kind() 的分类映射。
func TestSOLOStreamErrorKind(t *testing.T) {
	cases := []struct {
		code int64
		want provider.ErrKind
	}{
		{1005, provider.ErrHardCredit},
		{1001, provider.ErrModelBlocked},
		{3004, provider.ErrSoftRate},
		{9074, provider.ErrSoftRate},
	}
	for _, c := range cases {
		e := &SOLOStreamError{Code: c.code, Msg: "x"}
		if got := e.Kind(); got != c.want {
			t.Errorf("code=%d Kind()=%v，期望 %v", c.code, got, c.want)
		}
	}
}

// itoa 避免为测试引入 strconv 之外的依赖。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

var _ = http.StatusOK // 保持与 Stream 内 Status 语义一致
