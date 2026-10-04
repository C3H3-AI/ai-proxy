package qoder

import (
	"github.com/rockswang/workbuddy-wild/internal/provider"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── 单账号并发限制 ────────────────────────────────────────────────

// TestAcquireSlotSerializesSameAccount —— 同一账号的槽位容量为 DefaultMaxConcurrency(1)，
// 未释放时第二个 acquire 必须阻塞（而非放行打满上游窗口触发 10605）。
func TestAcquireSlotSerializesSameAccount(t *testing.T) {
	c := New()
	release := c.acquireSlot("u1")

	done := make(chan struct{})
	go func() {
		r2 := c.acquireSlot("u1") // 应阻塞直到 release()
		r2()
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("同一账号的第二个槽位不应立即获得（并发上限失效）")
	case <-time.After(120 * time.Millisecond):
		// 预期：阻塞中
	}
	release() // 释放后应放行
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("释放后第二个槽位应获得")
	}
}

// TestAcquireSlotDifferentAccountsIndependent —— 不同账号互不阻塞。
func TestAcquireSlotDifferentAccountsIndependent(t *testing.T) {
	c := New()
	r1 := c.acquireSlot("u1")
	defer r1()
	done := make(chan struct{})
	go func() {
		r2 := c.acquireSlot("u2")
		r2()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("不同账号不应互相阻塞")
	}
}

// TestSetMaxConcurrency —— 调大上限后，同一账号可并发到新上限。
func TestSetMaxConcurrency(t *testing.T) {
	c := New()
	c.SetMaxConcurrency(3)
	var wg sync.WaitGroup
	var cur, max int32
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel := c.acquireSlot("u1")
			n := atomic.AddInt32(&cur, 1)
			// 用 CAS 更新最大值：避免读-改-写竞态（此前用 if+Store 在 -race 下报 DATA RACE）
			for {
				old := atomic.LoadInt32(&max)
				if n <= old || atomic.CompareAndSwapInt32(&max, old, n) {
					break
				}
			}
			time.Sleep(80 * time.Millisecond)
			atomic.AddInt32(&cur, -1)
			rel()
		}()
	}
	wg.Wait()
	if atomic.LoadInt32(&max) < 2 {
		t.Errorf("并发上限=3 时观测到的最大并发=%d，应 >=2", atomic.LoadInt32(&max))
	}
}

// TestSetMaxConcurrencyInvalidFallsBack —— 非法值（<=0）回落默认。
func TestSetMaxConcurrencyInvalidFallsBack(t *testing.T) {
	c := New()
	c.SetMaxConcurrency(0)
	c.slotsMu.Lock()
	got := c.maxConcur
	c.slotsMu.Unlock()
	if got != DefaultMaxConcurrency {
		t.Errorf("maxConcur=%d want %d", got, DefaultMaxConcurrency)
	}
}

// ── 10605 识别 ────────────────────────────────────────────────────

// TestClassifyConcurrencyRejectionIsPassthrough —— 核心回归。
//
// 10605 是上游按账号并发窗口拒绝，账号本身健康。若归为 ErrSoftRate
// 会冷却整个账号 —— 把一个健康账号拖进冷却（与 #53 同型问题）。
func TestClassifyConcurrencyRejectionIsPassthrough(t *testing.T) {
	for _, body := range []string{
		`{"code":10605,"message":"concurrency limit exceeded"}`,
		`{"code":"10605","message":"busy"}`,
		`10605`,
	} {
		got := Classify(200, body)
		if got != provider.ErrPassthrough {
			t.Errorf("Classify(200,%q)=%v want ErrPassthrough", body, got)
		}
	}
}

// TestClassifyNormal429StillSoftRate —— 普通 429（账号级限流）不得被 10605 判据误吞。
func TestClassifyNormal429StillSoftRate(t *testing.T) {
	for _, body := range []string{
		``,
		`{"msg":"rate limit exceeded"}`,
		`{"code":429}`,
	} {
		if got := Classify(429, body); got != provider.ErrSoftRate {
			t.Errorf("Classify(429,%q)=%v want ErrSoftRate", body, got)
		}
	}
}

// TestErrPassthroughNotPenalized —— 语义锁定。
func TestErrPassthroughNotPenalized(t *testing.T) {
	if provider.ErrPassthrough.PenalizesAccount() {
		t.Error("ErrPassthrough 不应罚账号（账号健康，只是并发打满）")
	}
	if !provider.ErrSoftRate.PenalizesAccount() {
		t.Error("ErrSoftRate 应罚账号")
	}
}
