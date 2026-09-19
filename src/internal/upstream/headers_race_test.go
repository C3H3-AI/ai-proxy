package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/auth"
)

// TestRefreshHeadersNoSelfDeadlock —— 回归测试：持锁路径的自锁死锁。
//
// 背景（实测踩到过）：Client.RefreshToken 内部持有 a 的**写锁**，
// 随后调用 RefreshHeaders。若 RefreshHeaders 里改用加锁快照
// （a.RefreshTokenValue() 会再取读锁），就会自己等自己 → 死锁，
// 表现为 TestRefreshSuccess 挂起直到测试超时（实测 3min）。
//
// 本测试用短超时快速失败的方式锁定该契约。
func TestRefreshHeadersNoSelfDeadlock(t *testing.T) {
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", EnterpriseID: "e1"}

	done := make(chan string, 1)
	go func() {
		// 模拟 RefreshToken 的持锁路径
		a.Lock()
		defer a.Unlock()
		req := httptest.NewRequest(http.MethodPost, "https://example.com/refresh", nil)
		RefreshHeaders(req, a)
		done <- req.Header.Get("X-Refresh-Token")
	}()

	select {
	case got := <-done:
		if got != "rt" {
			t.Errorf("X-Refresh-Token = %q, want rt", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RefreshHeaders 在持锁路径下死锁（锁内不得调用加锁快照方法）")
	}
}

// TestHeadersUseLockedSnapshotNoRace —— 出站头取 token 必须走锁内快照。
//
// 修复前 ChatHeaders/BillingHeaders 直读 a.AccessToken，
// 与 keepalive 刷新并发时会触发 DATA RACE（-race 下可复现）。
// 本测试在 -race 下并发构造头与改写字段，锁定该契约。
func TestHeadersUseLockedSnapshotNoRace(t *testing.T) {
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", EnterpriseID: "e1"}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 写者：模拟 refresh / keepalive 持续改写 token
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 3000; i++ {
			a.Lock()
			a.AccessToken = "at" + strings.Repeat("x", i%8)
			a.UID = "u1"
			a.Unlock()
		}
		close(stop)
	}()

	// 读者：并发构造出站头（真实请求路径）
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				req := httptest.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
				ChatHeaders(req, a)
				req2 := httptest.NewRequest(http.MethodPost, "https://example.com/v2/billing/meter", nil)
				BillingHeaders(req2, a)
			}
		}()
	}

	wg.Wait()
}

// TestRefreshHeadersStillWorks —— 修复死锁后功能仍正确。
func TestRefreshHeadersStillWorks(t *testing.T) {
	a := &auth.Auth{UID: "u1", RefreshToken: "myrt", EnterpriseID: "ent9"}
	req := httptest.NewRequest(http.MethodPost, "https://example.com/refresh", nil)
	RefreshHeaders(req, a)

	if got := req.Header.Get("X-Refresh-Token"); got != "myrt" {
		t.Errorf("X-Refresh-Token=%q", got)
	}
	if got := req.Header.Get("X-Enterprise-Id"); got != "ent9" {
		t.Errorf("X-Enterprise-Id=%q", got)
	}
}

// TestChatHeadersUsesSnapshotValue —— ChatHeaders 必须反映当前 token（快照语义正确）。
func TestChatHeadersUsesSnapshotValue(t *testing.T) {
	a := &auth.Auth{UID: "u9", AccessToken: "AT-1"}
	req := httptest.NewRequest(http.MethodPost, "https://example.com/v2/chat", nil)
	ChatHeaders(req, a)

	if got := req.Header.Get("Authorization"); got != "Bearer AT-1" {
		t.Errorf("Authorization=%q", got)
	}
	if got := req.Header.Get("X-User-Id"); got != "u9" {
		t.Errorf("X-User-Id=%q", got)
	}

	// 空 token 时应走 X-No-Authorization 约定
	b := &auth.Auth{UID: "u9"}
	req2 := httptest.NewRequest(http.MethodPost, "https://example.com/v2/chat", nil)
	ChatHeaders(req2, b)
	if got := req2.Header.Get("X-No-Authorization"); got != "1" {
		t.Errorf("X-No-Authorization=%q", got)
	}
}

// TestTruncateGuardsNonPositiveN —— 上游 issue #146 同款：n<=0 不得 panic。
func TestTruncateGuardsNonPositiveN(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"hello", 0, ""},
		{"hello", -1, ""},
		{"hello", -100, ""},
		{"hello", 3, "hel"},
		{"hello", 10, "hello"},
		{"  padded  ", 6, "padded"},
	}
	for _, c := range cases {
		got := truncate(c.in, c.n) // n<=0 时若不加守卫会 panic
		if got != c.want {
			t.Errorf("truncate(%q,%d)=%q want %q", c.in, c.n, got, c.want)
		}
	}
}
