package traework

import (
	"net/http"
	"testing"

	"github.com/rockswang/workbuddy-wild/internal/provider"
)

// TestClassifyStatusBeatsBodyHardCredit —— 核心回归（P0）。
//
// 状态码必须优先于 body 里的 1005 硬余额标记：
// 上游报错格式不受本端控制，429/401 的响应体**同样携带业务码 1005**。
//
// 误判危害：
//   - 429 + 1005 → 误判 ErrHardCredit → **12h 硬冷却**（本该是软限流换号）
//   - 401 + 1005 → 误判 ErrHardCredit → **绕过 ErrSessionDead 自愈重登**，死号永久不可用
//
// 这还会与已移植的 PR #82 换号机制叠加放大：
// 换号决策按 ErrKind 走，判据错则整条链路都错。
func TestClassifyStatusBeatsBodyHardCredit(t *testing.T) {
	cases := []struct {
		name string
		stat int
		body string
		want provider.ErrKind
	}{
		// ── 状态码必须赢过 body 的 1005 ──
		{"429 带 1005 仍判限流", http.StatusTooManyRequests, `{"code":1005,"msg":"too many requests"}`, provider.ErrSoftRate},
		{"401 带 1005 仍判登录失效", http.StatusUnauthorized, `{"code":1005,"msg":"token 失效"}`, provider.ErrSessionDead},
		// 403 无专门状态码分支，会落到 1005 判据 → 硬余额。
		// 这是合理的：403 常与配额/权限额度一同出现，且上游同样按 1005 判余额。
		{"403 带 1005 落 1005 判据", http.StatusForbidden, `{"code":1005}`, provider.ErrHardCredit},

		// ── 无状态码可依据时，1005 仍判硬余额 ──
		{"402 直判硬余额", http.StatusPaymentRequired, `{"code":1005}`, provider.ErrHardCredit},
		{"200 body 带 1005 判硬余额", http.StatusOK, `{"code":1005,"msg":"积分不足"}`, provider.ErrHardCredit},
		{"200 plan+1005 判硬余额", http.StatusOK, `your plan limit 1005 reached`, provider.ErrHardCredit},

		// ── 其他状态码不受影响 ──
		{"429 无 1005 判限流", http.StatusTooManyRequests, `rate limited`, provider.ErrSoftRate},
		{"404 判未找到", http.StatusNotFound, `no route`, provider.ErrNotFound},
		{"500 判服务端错误", http.StatusInternalServerError, `oops`, provider.ErrServer},
		{"400 兜底判客户端错误", http.StatusBadRequest, `{"msg":"bad"}`, provider.ErrClient},
		{"200 正常", http.StatusOK, `{"ok":true}`, provider.ErrNone},
	}
	for _, c := range cases {
		if got := Classify(c.stat, c.body); got != c.want {
			t.Errorf("%s：Classify(%d,%q)=%v，期望 %v", c.name, c.stat, c.body, got, c.want)
		}
	}
}

// TestClassify401AlwaysSessionDead —— 401 无论 body 是什么都判登录失效
// （这是自愈重登的依据，不能被任何 body 标记覆盖）。
func TestClassify401AlwaysSessionDead(t *testing.T) {
	for _, body := range []string{
		``,
		`{"code":1005,"msg":"whatever"}`,
		`{"code":11102,"msg":"no such model"}`,
		`{"code":1005}`, // 关键：含 1005 也必须判 session dead
	} {
		if got := Classify(http.StatusUnauthorized, body); got != provider.ErrSessionDead {
			t.Errorf("401 body=%q 判为 %v，应为 ErrSessionDead", body, got)
		}
	}
}

// TestClassify429NeverHardCredit —— 限流绝不判硬余额（否则账号被 12h 冻死）。
func TestClassify429NeverHardCredit(t *testing.T) {
	for _, body := range []string{
		`{"code":1005}`,
		`{"code":1005,"msg":"quota"}`,
		`plan 1005 limit`,
		`{"code":6004,"msg":"将在 2026-09-23 14:47:19 UTC+8 重置"}`,
	} {
		got := Classify(http.StatusTooManyRequests, body)
		if got == provider.ErrHardCredit {
			t.Errorf("429 body=%q 被误判为硬余额（会导致 12h 硬冷却）", body)
		}
		if got != provider.ErrSoftRate {
			t.Errorf("429 body=%q 判为 %v，应为 ErrSoftRate", body, got)
		}
	}
}

// TestClassifyRequestLevelErrorsUnaffected —— 请求级错误分类不受本次顺序调整影响。
func TestClassifyRequestLevelErrorsUnaffected(t *testing.T) {
	if got := Classify(http.StatusBadRequest, `{"code":11115,"msg":"prompt too long"}`); got != provider.ErrPromptTooLong {
		t.Errorf("11115 应判 ErrPromptTooLong，实际 %v", got)
	}
	if got := Classify(http.StatusOK, `blocked by security policy`); got != provider.ErrContentBlocked {
		t.Errorf("内容拦截文案应判 ErrContentBlocked，实际 %v", got)
	}
}
