package main

import (
	"testing"

	"github.com/rockswang/workbuddy-wild/internal/auth"
	"github.com/rockswang/workbuddy-wild/internal/config"
	"github.com/rockswang/workbuddy-wild/internal/pool"
	"github.com/rockswang/workbuddy-wild/internal/provider"
	"github.com/rockswang/workbuddy-wild/internal/svc"
)

// newTestRuntime 构造一个只含 WorkBuddy 池的 Runtime（写临时 state 文件），
// 用于验证 ctl 的批量语义。
func newTestRuntime(t *testing.T, auths []*auth.Auth) *svc.Runtime {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		AuthDir:            dir,
		StateFile:          dir + "/state.json",
		LowCreditThreshold: 10,
	}
	r := &svc.Runtime{Config: cfg}
	p := pool.New(dir + "/state-workbuddy.json")
	p.SetLowCredits(cfg.LowCreditThreshold)
	for _, a := range auths {
		p.Add(a)
	}
	r.WorkBuddyPool = p
	r.WorkBuddyAccounts = auths
	return r
}

func mkAuth(uid string, credits int64) *auth.Auth {
	// 注意：credits 不在 auth.Auth 上，由 pool 维护（SetCredits）。
	return &auth.Auth{UID: uid, AccessToken: "at-" + uid, RefreshToken: "rt-" + uid}
}

// TestCollectSetEnabledBatch —— 批量启用/禁用的核心语义（面板「全部启用/禁用」依赖它）。
//
// 关键：uid=="" 表示对**所有**账号生效（ctl 的 collect* 系列都用
// `if uid != "" && st.UID != uid` 做跳过判断）。
func TestCollectSetEnabledBatch(t *testing.T) {
	r := newTestRuntime(t, []*auth.Auth{mkAuth("u1", 100), mkAuth("u2", 200)})
	pl := r.Pool(provider.WorkBuddy)

	// 先全部禁用
	out := collectSetEnabled(r, []provider.Kind{provider.WorkBuddy}, "", false)
	if len(out) != 2 {
		t.Fatalf("批量禁用应覆盖 2 个账号，实际 %d", len(out))
	}
	for _, o := range out {
		if !o.OK {
			t.Errorf("uid=%s 禁用失败: %s", o.UID, o.Msg)
		}
	}
	for _, uid := range []string{"u1", "u2"} {
		st, ok := pl.Status(uid)
		if !ok || !st.Disabled {
			t.Errorf("uid=%s 应处于禁用状态: %+v", uid, st)
		}
	}

	// 再全部启用（uid=""）
	out = collectSetEnabled(r, []provider.Kind{provider.WorkBuddy}, "", true)
	if len(out) != 2 {
		t.Fatalf("批量启用应覆盖 2 个账号，实际 %d", len(out))
	}
	for _, uid := range []string{"u1", "u2"} {
		st, ok := pl.Status(uid)
		if !ok || st.Disabled {
			t.Errorf("uid=%s 应已恢复启用: %+v", uid, st)
		}
	}
}

// TestCollectSetEnabledSingleUid —— 指定 uid 时只影响该账号。
func TestCollectSetEnabledSingleUid(t *testing.T) {
	r := newTestRuntime(t, []*auth.Auth{mkAuth("u1", 100), mkAuth("u2", 200)})
	pl := r.Pool(provider.WorkBuddy)

	out := collectSetEnabled(r, []provider.Kind{provider.WorkBuddy}, "u1", false)
	if len(out) != 1 || out[0].UID != "u1" {
		t.Fatalf("指定 uid 时只应处理 1 个: %+v", out)
	}
	if st, _ := pl.Status("u2"); st.Disabled {
		t.Error("u2 不应被影响")
	}
}

// TestCollectSetEnabledIdempotent —— 重复操作应给出明确提示而非误报成功。
func TestCollectSetEnabledIdempotent(t *testing.T) {
	r := newTestRuntime(t, []*auth.Auth{mkAuth("u1", 100)})
	// 账号本就未禁用 → enable 应提示"无需启用"
	out := collectSetEnabled(r, []provider.Kind{provider.WorkBuddy}, "u1", true)
	if len(out) != 1 {
		t.Fatalf("应返回 1 条结果: %+v", out)
	}
	if out[0].OK {
		t.Errorf("未禁用账号执行 enable 不应标记 OK，实际 msg=%q", out[0].Msg)
	}
}

// TestCollectUnlockSkipsPermanentlyDisabled —— 永久禁用账号不可解锁
// （面板「全部解锁」时必须跳过，且要给出明确原因）。
func TestCollectUnlockSkipsPermanentlyDisabled(t *testing.T) {
	r := newTestRuntime(t, []*auth.Auth{mkAuth("u1", 100), mkAuth("u2", 5)})
	pl := r.Pool(provider.WorkBuddy)
	pl.Disable("u1", "session dead") // 永久禁用
	pl.SetCredits("u2", 5)
	pl.ReenableIfCredits("u2", 5) // 低于阈值 10 → 标记低积分

	out := collectUnlock(r, []provider.Kind{provider.WorkBuddy}, "")
	var u1, u2 *outUnlock
	for i := range out {
		switch out[i].UID {
		case "u1":
			u1 = &out[i]
		case "u2":
			u2 = &out[i]
		}
	}
	if u1 == nil || u2 == nil {
		t.Fatalf("应返回两个账号的结果: %+v", out)
	}
	if u1.OK {
		t.Error("永久禁用账号不应解锁成功")
	}
	if !u2.OK {
		t.Errorf("低积分账号应解锁成功，实际 msg=%q", u2.Msg)
	}
}

// TestWantKind —— kind 过滤：空表示全部渠道，指定则单渠道。
func TestWantKind(t *testing.T) {
	if got := len(wantKind("")); got != len(kinds) {
		t.Errorf("空 platform 应返回全部渠道，实际 %d/%d", got, len(kinds))
	}
	one := wantKind("workbuddy")
	if len(one) != 1 || one[0] != provider.WorkBuddy {
		t.Errorf("指定 workbuddy 应只返回该渠道: %v", one)
	}
	if got := wantKind("nonexistent"); got != nil {
		t.Errorf("未知渠道应返回 nil: %v", got)
	}
}
