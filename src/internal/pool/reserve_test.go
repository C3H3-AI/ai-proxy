package pool

import (
	"testing"

	"github.com/rockswang/workbuddy-wild/internal/auth"
)

func mkPoolWithCredits(t *testing.T, reserve int64, credits map[string]int64) *Pool {
	t.Helper()
	p := New("")
	for uid, c := range credits {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at"})
		p.SetCredits(uid, c)
	}
	p.SetReserveCredits(reserve)
	p.SetLowCredits(0) // 隔离低积分阈值，只测保留线
	return p
}

// TestReserveCreditsKeepsBottom —— 核心：付费选号只从「积分 > 保留线」的账号中选，
// 落到保留线的账号留作底仓。
func TestReserveCreditsKeepsBottom(t *testing.T) {
	p := mkPoolWithCredits(t, 50, map[string]int64{
		"high":  200,
		"edge":  50, // 恰好等于保留线 → 应被排除
		"below": 20, // 低于保留线 → 应被排除
	})

	got := p.Pick() // Pick 是付费路径（排除低积分）
	if got == nil {
		t.Fatal("应能选到账号")
	}
	if got.UID != "high" {
		t.Errorf("应只选积分 > 保留线的 high，实际=%s", got.UID)
	}
}

// TestReserveCreditsOffByDefault —— 保留线=0（默认）时行为不变：
// 所有账号照旧参与付费轮转，选积分最多的。
func TestReserveCreditsOffByDefault(t *testing.T) {
	p := mkPoolWithCredits(t, 0, map[string]int64{"a": 10, "b": 100})
	got := p.Pick()
	if got == nil || got.UID != "b" {
		t.Fatalf("保留线关闭时应选积分最多的 b，实际=%v", got)
	}
}

// TestReserveCreditsDoesNotAffectFreePath —— 保留线**只作用于付费路径**。
//
// 验证方式（可证、不依赖「低分账号必被 lowCredit 排除」的巧合）：
// 同一组账号分别在 reserve=0 与 reserve=50 下调用 PickLowCredit，
// 结果必须**完全一致** —— 即保留线开关不改变免费路径的行为。
func TestReserveCreditsDoesNotAffectFreePath(t *testing.T) {
	build := func(reserve int64) *Pool {
		p := New("")
		for uid, c := range map[string]int64{"rich": 200, "mid": 40, "low": 3} {
			p.Add(&auth.Auth{UID: uid, AccessToken: "at"})
			p.SetCredits(uid, c)
		}
		p.SetLowCredits(10) // low=3 为 lowCredit
		p.SetReserveCredits(reserve)
		return p
	}
	off := build(0)
	on := build(50)

	gotOff := off.PickLowCredit()
	gotOn := on.PickLowCredit()

	if (gotOff == nil) != (gotOn == nil) {
		t.Fatalf("保留线改变了免费路径的可用性：off=%v on=%v", gotOff, gotOn)
	}
	if gotOff != nil && gotOn != nil && gotOff.UID != gotOn.UID {
		t.Errorf("保留线改变了免费路径的选号：off=%s on=%s", gotOff.UID, gotOn.UID)
	}
	// 付费路径则应受影响：reserve=50 时排除低于 50 的账号
	if got := on.Pick(); got == nil || got.UID != "rich" {
		t.Errorf("付费应只选 rich（>50），实际=%v", got)
	}
}

// TestReserveCreditsAllBelowFallsBack —— 全部账号都低于保留线时：
// 付费请求应回退到「有积分最多的那个」，而不是直接报无可用账号
// （否则用户会突然完全不可用）。
func TestReserveCreditsAllBelowFallsBack(t *testing.T) {
	p := New("")
	for uid, c := range map[string]int64{"a": 10, "b": 30} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at"})
		p.SetCredits(uid, c)
	}
	p.SetLowCredits(0)
	p.SetReserveCredits(100) // 高于所有账号

	got := p.Pick()
	if got == nil {
		t.Fatal("全部低于保留线时不应完全不可用（应回退）")
	}
	if got.UID != "b" {
		t.Errorf("回退时应选积分最多的 b，实际=%s", got.UID)
	}
}

// TestSetReserveCreditsClampsNegative —— 负值应回落 0（关闭），
// 避免「-1 保留线」把一切过滤掉导致完全不可用。
func TestSetReserveCreditsClampsNegative(t *testing.T) {
	p := New("")
	p.SetReserveCredits(-5)
	p.mu.RLock()
	got := p.reserveCredits
	p.mu.RUnlock()
	if got != 0 {
		t.Errorf("负值应回落 0，实际=%d", got)
	}
}
