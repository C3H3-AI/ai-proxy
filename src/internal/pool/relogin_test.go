package pool

import (
	"testing"

	"github.com/rockswang/workbuddy-wild/internal/auth"
)

// TestAddReplacesCredentialsClearsDisabled —— 核心回归。
//
// 事故：账号 session 死亡被 Disable 后，用户重新登录覆盖了 auth 文件
// （新 accessToken），但 state.json 里的 disabled 仍在 →
// 面板一直显示"启用无效"并引导删除，其实凭证已可用。
//
// 修法：Add 时若 accessToken 变了且账号处于 disabled，自动解除禁用。
func TestAddReplacesCredentialsClearsDisabled(t *testing.T) {
	p := New("")
	uid := "u1"

	// 先加入旧凭证
	p.Add(&auth.Auth{UID: uid, AccessToken: "old-token"})
	// 模拟 session 死亡被禁用
	p.Disable(uid, "session dead")
	if st, ok := p.Status(uid); !ok || !st.Disabled {
		t.Fatalf("前置条件失败：应处于 disabled，实际 %+v", st)
	}

	// 用户重新登录 → 新凭证
	p.Add(&auth.Auth{UID: uid, AccessToken: "new-token"})

	st, _ := p.Status(uid)
	if st.Disabled {
		t.Fatalf("凭证更新后仍 disabled（reason=%s），应自动解除", st.Reason)
	}
	if st.Reason != "" {
		t.Errorf("解除后 reason 应清空，实际 %q", st.Reason)
	}
}

// TestAddSameCredentialsKeepsDisabled —— 凭证没变（仅目录重新扫描）时
// 不应无谓重置 disabled，否则会掩盖"账号确实失效"这一事实。
func TestAddSameCredentialsKeepsDisabled(t *testing.T) {
	p := New("")
	uid := "u1"

	p.Add(&auth.Auth{UID: uid, AccessToken: "same-token"})
	p.Disable(uid, "session dead")

	// 同一凭证再次 Add（例如定时扫描目录）
	p.Add(&auth.Auth{UID: uid, AccessToken: "same-token"})

	st, _ := p.Status(uid)
	if !st.Disabled {
		t.Error("凭证未变时不应解除 disabled（会掩盖真实失效）")
	}
}

// TestAddNewAccountNotDisabled —— 全新账号加入时不应是禁用态。
func TestAddNewAccountNotDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "brand-new", AccessToken: "t"})
	st, ok := p.Status("brand-new")
	if !ok {
		t.Fatal("新账号应存在于池中")
	}
	if st.Disabled {
		t.Error("新账号不应为 disabled")
	}
}
