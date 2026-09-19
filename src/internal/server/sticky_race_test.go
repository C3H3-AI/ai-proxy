package server

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/auth"
	"github.com/rockswang/workbuddy-wild/internal/pool"
)

// TestStressConcurrentRotateAndStateChange —— 粘性路由的竞态回归测试。
//
// 背景（-race 下实测复现过）：pickWithSticky 原先只持读锁取出 *stickyEntry
// 指针，解锁后再读它的字段；而 stickySuccess 在写锁内改同一对象的 reqCount。
// 锁保护的是 map，不是 entry 对象 → DATA RACE。
//
// 修复：锁内把 uid/reqCount/maxReqs 拷贝出来。
// 本测试并发跑请求（触发 stickySuccess）与池状态改写，锁定该契约。
func TestStressConcurrentRotateAndStateChange(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})

	var wg sync.WaitGroup
	body := `{"model":"workbuddy/glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`

	// 请求者
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
				h.ServeHTTP(rec, req)
			}
		}()
	}
	// 状态改写者（模拟 keepalive / 签到并发）
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 200; j++ {
			p.SetCredits("u1", int64(j))
			p.NoteSuccess("u1")
			p.Cooldown("u2", pool.CoolSoft, time.Millisecond, "stress")
		}
	}()
	wg.Wait()
}
