package upstream

import (
	"net/http"
	"testing"

	"github.com/rockswang/workbuddy-wild/internal/provider"
)

// TestClassifyRequestSideErrorsNotAccountFaults —— 核心回归测试。
//
// 背景：Classify 原先对「内容拦截 / 上下文超限 / 请求体错误」一律落到
// ErrClient 兜底 → handler 调 Pool.NoteError → 连续 3 次把**健康账号**
// 冷却 10 分钟。这三类错误换任何账号结果都相同（是请求的问题，不是账号的），
// 罚账号既误伤又浪费其他账号的请求配额。
//
// 本测试锁定：它们必须被单独分类，且 provider.PenalizesAccount() 为 false。
func TestClassifyRequestSideErrorsNotAccountFaults(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   provider.ErrKind
	}{
		{
			"内容策略拦截（security policy）",
			http.StatusBadRequest,
			`{"code":1,"msg":"Your request was blocked by security policy"}`,
			ErrContentBlocked,
		},
		{
			"内容策略拦截（unapproved channel）",
			http.StatusBadRequest,
			`{"code":1,"msg":"unapproved channel"}`,
			ErrContentBlocked,
		},
		{
			"上下文超限（code 11115）",
			http.StatusBadRequest,
			`{"code":11115,"msg":"prompt is too long"}`,
			ErrPromptTooLong,
		},
		{
			"上下文超限（仅文案，大小写混合）",
			http.StatusBadRequest,
			`{"code":1,"msg":"Prompt Is Too Long"}`,
			ErrPromptTooLong,
		},
		{
			"请求体解析失败（11101）",
			http.StatusBadRequest,
			`{"code":11101,"msg":"Unmarshal chat params failed"}`,
			ErrBadParams,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Classify(c.status, c.body)
			if got != c.want {
				t.Fatalf("Classify=%v want %v（若为 client 说明被通用兜底吞掉）", got, c.want)
			}
			if got.PenalizesAccount() {
				t.Errorf("%v 不应罚账号（会误冷却健康账号）", got)
			}
		})
	}
}

// TestClassifyAccountFaultsPenalize —— 账号级故障必须仍被惩罚（防止矫枉过正）。
func TestClassifyAccountFaultsPenalize(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   provider.ErrKind
	}{
		{
			"账号级授权风控 11140",
			http.StatusForbidden,
			`{"code":11140,"msg":"request illegal"}`,
			ErrAccountFault,
		},
		{
			"试用未激活 14017",
			http.StatusForbidden,
			`{"code":14017,"msg":"The trial version is not yet activated"}`,
			ErrAccountFault,
		},
		{
			"WAF 拦截（403 空体）",
			http.StatusForbidden,
			"",
			ErrWafBlock,
		},
		{
			"WAF 拦截（403 + HTML 拦截页）",
			http.StatusForbidden,
			"<html><body>403 Forbidden</body></html>",
			ErrWafBlock,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Classify(c.status, c.body)
			if got != c.want {
				t.Fatalf("Classify=%v want %v", got, c.want)
			}
			if !got.PenalizesAccount() {
				t.Errorf("%v 应当罚账号（否则会持续给上游送死请求）", got)
			}
		})
	}
}

// TestClassifyExistingBehaviorIntact —— 确保新规则没破坏既有分类。
func TestClassifyExistingBehaviorIntact(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   provider.ErrKind
	}{
		{402, ``, ErrHardCredit},
		{400, `{"code":1,"msg":"余额不足"}`, ErrHardCredit},
		{403, `insufficient credits`, ErrHardCredit},
		{200, `{"code":10001,"msg":"积分不足，请充值"}`, ErrHardCredit},
		{429, ``, ErrSoftRate},
		{401, `Offline user session not found`, ErrSessionDead},
		{401, `{"code":12153,"msg":"Offline user session not found"}`, ErrSessionDead},
		{401, `{"code":9999,"msg":"bad token"}`, ErrClient},
		{500, `boom`, ErrServer},
		{503, `unavailable`, ErrServer},
		{200, ``, ErrNone},
		{404, ``, ErrNotFound},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d,%q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

// TestClassifyModelBlocked —— 11102「该后端无此模型」应可识别，便于按 (账号,模型) 避让。
func TestClassifyModelBlocked(t *testing.T) {
	got := Classify(http.StatusBadRequest, `{"code":11102,"msg":"model not supported"}`)
	if got != ErrModelBlocked {
		t.Errorf("Classify=%v want ErrModelBlocked", got)
	}
}

// TestErrKindPenalizesAccount —— 直接锁定 PenalizesAccount 的真值表。
func TestErrKindPenalizesAccount(t *testing.T) {
	noPenalty := []provider.ErrKind{
		provider.ErrContentBlocked, provider.ErrPromptTooLong, provider.ErrBadParams,
	}
	for _, k := range noPenalty {
		if k.PenalizesAccount() {
			t.Errorf("%v 不应罚账号", k)
		}
	}
	penalty := []provider.ErrKind{
		provider.ErrHardCredit, provider.ErrSoftRate, provider.ErrSessionDead,
		provider.ErrNotFound, provider.ErrServer, provider.ErrClient,
		provider.ErrWafBlock, provider.ErrAccountFault, provider.ErrModelBlocked,
	}
	for _, k := range penalty {
		if !k.PenalizesAccount() {
			t.Errorf("%v 应当罚账号", k)
		}
	}
}
