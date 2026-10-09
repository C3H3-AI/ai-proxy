package server

import (
	"testing"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/provider"
)

// ── Rotatable：区分「账号级错误换号」与「请求级错误透传」──────────

// TestRotatableAccountErrors —— 账号有问题的错误应换号重试。
func TestRotatableAccountErrors(t *testing.T) {
	for _, k := range []provider.ErrKind{
		provider.ErrSoftRate, provider.ErrHardCredit, provider.ErrSessionDead,
		provider.ErrNotFound, provider.ErrServer, provider.ErrClient,
		provider.ErrWafBlock, provider.ErrAccountFault, provider.ErrModelBlocked,
	} {
		if !k.Rotatable() {
			t.Errorf("kind=%v 是账号级错误，应可换号重试", k)
		}
	}
}

// TestRotatableRequestErrors —— 核心回归：请求本身有问题的错误
// 【不应换号】，否则换任何账号都必然复现，白白轮转消耗其他账号配额，
// 客户端还会收到本可避免的 503。
func TestRotatableRequestErrors(t *testing.T) {
	for _, k := range []provider.ErrKind{
		provider.ErrContentBlocked, // 内容策略拦截
		provider.ErrPromptTooLong,  // 上下文超限
	} {
		if k.Rotatable() {
			t.Errorf("kind=%v 是请求级错误，换号必然复现，不应换号而应透传", k)
		}
	}
}

// TestRotatableBadParamsStillRotates —— ErrBadParams 的定义注释为
// "不罚号，但仍轮转"，故 Rotatable 应为 true（与 PromptTooLong 形成对照）。
func TestRotatableBadParamsStillRotates(t *testing.T) {
	if !provider.ErrBadParams.Rotatable() {
		t.Error("ErrBadParams 按其定义注释应仍轮转（true）")
	}
	if provider.ErrPromptTooLong.Rotatable() {
		t.Error("ErrPromptTooLong 按其定义注释为「不轮转」（false）")
	}
}

// TestRotatableConsistentWithPenalizes —— 两者互补但判定对象不同：
// 请求级错误既不罚账号、也不换号。这里锁定二者不矛盾。
func TestRotatableConsistentWithPenalizes(t *testing.T) {
	for _, k := range []provider.ErrKind{provider.ErrContentBlocked, provider.ErrPromptTooLong} {
		if k.PenalizesAccount() {
			t.Errorf("kind=%v 不应罚账号", k)
		}
		if k.Rotatable() {
			t.Errorf("kind=%v 不应换号", k)
		}
	}
}

// ── softRateResetTs：从 6004 响应解析真实重置时间 ─────────────────

// TestSoftRateResetTsParses —— 命中「将在 <ts> 重置」应解析出 UTC+8 墙钟时间。
func TestSoftRateResetTsParses(t *testing.T) {
	body := `{"code":6004,"msg":"当前额度已用完，将在 2026-09-23 14:47:19 UTC+8 重置"}`
	ts, ok := softRateResetTs(body)
	if !ok {
		t.Fatal("应解析出重置时间")
	}
	// 期望：UTC+8 的 2026-09-23 14:47:19
	want := time.Date(2026, 9, 23, 14, 47, 19, 0, time.FixedZone("UTC+8", 8*3600))
	if !ts.Equal(want) {
		t.Errorf("解析=%v want=%v", ts, want)
	}
	// 换算成 UTC 应为 06:47:19，验证时区确实按 UTC+8 处理
	if ts.UTC().Hour() != 6 {
		t.Errorf("UTC 小时=%d，应按时区偏移后为 6", ts.UTC().Hour())
	}
}

// TestSoftRateResetTsMiss —— 未命中形态应返回 false（回退默认冷却）。
func TestSoftRateResetTsMiss(t *testing.T) {
	for _, body := range []string{
		`{"code":429,"msg":"rate limit"}`,
		``,
		`将在 `,                     // 缺「 重置」
		`重置`,                      // 缺「将在 」
		`将在 not-a-time 重置`,       // 时间格式非法
	} {
		if _, ok := softRateResetTs(body); ok {
			t.Errorf("body=%q 不应解析出重置时间", body)
		}
	}
}

// TestSoftRateCooldownUsesResetTime —— 核心：命中重置时间时冷却时长
// 应接近到重置点的间隔，而不是固定 SoftCooldown。
func TestSoftRateCooldownUsesResetTime(t *testing.T) {
	h := &Handler{}
	h.cfg.SoftCooldown = 60 * time.Second

	// 构造一个 2 小时后的重置点（UTC+8 墙钟）
	reset := time.Now().In(time.FixedZone("UTC+8", 8*3600)).Add(2 * time.Hour)
	s := reset.Format("2006-01-02 15:04:05")
	body := "额度已用完，将在 " + s + " UTC+8 重置"

	got := h.softRateCooldown(body)
	if got <= 60*time.Second {
		t.Errorf("应按重置点冷却（约 2h），实际=%v", got)
	}
	if got > 2*time.Hour+time.Minute {
		t.Errorf("冷却过长=%v", got)
	}
}

// TestSoftRateCooldownFallsBack —— 解析失败时回退默认 SoftCooldown（不改变原行为）。
func TestSoftRateCooldownFallsBack(t *testing.T) {
	h := &Handler{}
	h.cfg.SoftCooldown = 60 * time.Second
	if got := h.softRateCooldown(`{"code":429,"msg":"rate limit"}`); got != 60*time.Second {
		t.Errorf("应回退默认 SoftCooldown，实际=%v", got)
	}
}

// TestSoftRateCooldownCap —— 异常时间戳（远超上限）不应把号冻死。
func TestSoftRateCooldownCap(t *testing.T) {
	h := &Handler{}
	h.cfg.SoftCooldown = 60 * time.Second
	// 一年后（超过 24h 上限）
	reset := time.Now().In(time.FixedZone("UTC+8", 8*3600)).Add(365 * 24 * time.Hour)
	body := "将在 " + reset.Format("2006-01-02 15:04:05") + " UTC+8 重置"
	got := h.softRateCooldown(body)
	if got > softRateResetMaxCap {
		t.Errorf("冷却应被上限截断，got=%v cap=%v", got, softRateResetMaxCap)
	}
}

// TestSoftRateCooldownPastReset —— 重置点已过去（d<=0）应回退默认值。
func TestSoftRateCooldownPastReset(t *testing.T) {
	h := &Handler{}
	h.cfg.SoftCooldown = 60 * time.Second
	past := time.Now().In(time.FixedZone("UTC+8", 8*3600)).Add(-1 * time.Hour)
	body := "将在 " + past.Format("2006-01-02 15:04:05") + " UTC+8 重置"
	if got := h.softRateCooldown(body); got != 60*time.Second {
		t.Errorf("重置点已过应回退默认，got=%v", got)
	}
}

// ── truncateBody ──────────────────────────────────────────────

func TestTruncateBody(t *testing.T) {
	if got := truncateBody([]byte("short")); got != "short" {
		t.Errorf("短 body 不应截断，got=%q", got)
	}
	long := make([]byte, 500)
	for i := range long {
		long[i] = 'x'
	}
	if got := truncateBody(long); len(got) != 200 {
		t.Errorf("长 body 应截断到 200，实际 len=%d", len(got))
	}
}
