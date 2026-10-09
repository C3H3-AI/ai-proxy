package scheduler

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/auth"
	"github.com/rockswang/workbuddy-wild/internal/pool"
	"github.com/rockswang/workbuddy-wild/internal/provider"
)

// fakeUp 无副作用的桩上游。
type fakeUp struct{}

func (fakeUp) RefreshToken(*auth.Auth) error                { return nil }
func (fakeUp) DailyCheckin(*auth.Auth) error                 { return nil }
func (fakeUp) UserResource(*auth.Auth) (int64, error)        { return 100, nil }
func (fakeUp) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return 100, nil, nil
}
func (fakeUp) ChatStream(*auth.Auth, []byte) (io.ReadCloser, int, []byte, error) {
	return nil, 0, nil, nil
}
func (fakeUp) Classify(int, string) provider.ErrKind              { return provider.ErrNone }
func (fakeUp) FetchModels(*auth.Auth) ([]provider.ModelInfo, error) { return nil, nil }
func (fakeUp) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) { return nil, nil }
func (fakeUp) Stream(http.ResponseWriter, io.Reader) error        { return nil }
func (fakeUp) Aggregate(io.Reader) (map[string]any, error)        { return nil, nil }

func newTestPool() *pool.Pool { return pool.New("") }

// TestNewNilCheckinUsesDefault —— CheckinMinutes 为 nil（未配置）时应填默认时刻。
func TestNewNilCheckinUsesDefault(t *testing.T) {
	s := New(Config{Pool: newTestPool(), Upstream: fakeUp{}, Name: "x", CheckinMinutes: nil})
	ch, _ := s.schedule()
	if len(ch) == 0 {
		t.Fatal("nil 应被兜底为默认签到时刻")
	}
	// 默认应包含 0 / 9:00 / 21:00
	has := func(v int) bool {
		for _, m := range ch {
			if m == v {
				return true
			}
		}
		return false
	}
	if !has(0) || !has(9*60) || !has(21*60) {
		t.Errorf("默认签到时刻应含 0/9:00/21:00，实际=%v", ch)
	}
}

// TestNewEmptyCheckinDisables —— 核心回归：显式空切片 []int{}
// 表示「本渠道无签到活动」，**不得**被兜底成默认时刻。
//
// 背景：qoder 上游没有签到接口。此前用 len()==0 判断，
// 显式空切片也被兜底成默认时刻 → qoder 每天白跑两次注定失败的签到。
func TestNewEmptyCheckinDisables(t *testing.T) {
	s := New(Config{Pool: newTestPool(), Upstream: fakeUp{}, Name: "qoder", CheckinMinutes: []int{}})
	ch, _ := s.schedule()
	if len(ch) != 0 {
		t.Errorf("显式空切片应保持为空（不排程签到），实际=%v", ch)
	}
}

// TestNewNilKeepaliveUsesDefault —— KeepaliveHours 为 nil 时填默认 22 点。
func TestNewNilKeepaliveUsesDefault(t *testing.T) {
	s := New(Config{Pool: newTestPool(), Upstream: fakeUp{}, Name: "x", CheckinMinutes: []int{}, KeepaliveHours: nil})
	_, kh := s.schedule()
	if len(kh) != 1 || kh[0] != 22 {
		t.Errorf("KeepaliveHours=nil 应兜底为 [22]，实际=%v", kh)
	}
}

// TestNewEmptyKeepaliveDisables —— 显式空切片应保持为空。
func TestNewEmptyKeepaliveDisables(t *testing.T) {
	s := New(Config{Pool: newTestPool(), Upstream: fakeUp{}, Name: "x",
		CheckinMinutes: []int{}, KeepaliveHours: []int{}})
	_, kh := s.schedule()
	if len(kh) != 0 {
		t.Errorf("显式空切片应保持为空，实际=%v", kh)
	}
}

// TestRunNoTasksDoesNotSpin —— 核心：两列表都为空时，Run 应阻塞等待，
// 而不是每分钟空转一次。
//
// 若不保护，nextFireMinutes 返回零值 → time.Until(零值) 为很大的负数
// → timer 立即触发 → 每天 1440 次空转（白白消耗 CPU）。
func TestRunNoTasksDoesNotSpin(t *testing.T) {
	s := New(Config{Pool: newTestPool(), Upstream: fakeUp{}, Name: "idle",
		CheckinMinutes: []int{}, KeepaliveHours: []int{}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	// 若在空转，2 秒内会被唤醒很多次；这里只验证它**仍在阻塞**（未被取消）
	select {
	case <-done:
		t.Fatal("Run 不应自行退出")
	case <-time.After(1500 * time.Millisecond):
		// 仍在运行即符合预期
	}

	// 取消后应正常退出
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("取消后 Run 应退出")
	}
}

// TestRunWithTasksStillSchedules —— 有任务时排程仍正常（不被空保护误伤）。
func TestRunWithTasksStillSchedules(t *testing.T) {
	s := New(Config{Pool: newTestPool(), Upstream: fakeUp{}, Name: "x",
		CheckinMinutes: []int{9 * 60, 21 * 60}, KeepaliveHours: []int{22}})

	ch, kh := s.schedule()
	if len(ch) != 2 {
		t.Errorf("应保留配置的 2 个签到时刻，实际=%v", ch)
	}
	if len(kh) != 1 || kh[0] != 22 {
		t.Errorf("应保留配置的保活小时，实际=%v", kh)
	}
}
