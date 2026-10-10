package scheduler

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/auth"
	"github.com/rockswang/workbuddy-wild/internal/ledger"
	"github.com/rockswang/workbuddy-wild/internal/pool"
	"github.com/rockswang/workbuddy-wild/internal/provider"
)

// fakeUpstreamForLedger 假上游：签到成功、资源明细带到期日。
type fakeUpstreamForLedger struct{}

func (fakeUpstreamForLedger) RefreshToken(*auth.Auth) error          { return nil }
func (fakeUpstreamForLedger) DailyCheckin(*auth.Auth) error          { return nil }
func (fakeUpstreamForLedger) UserResource(*auth.Auth) (int64, error) { return 100, nil }

// UserResourceDetail 返回带到期日的条目 —— 有 ExpireAt 才能区分「到期作废」与「消耗」。
func (fakeUpstreamForLedger) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	// 用「明天」避免测试随系统日期腐烂（参考 ledger 侧的同类修复）
	exp := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	return 100, []provider.ResourceItem{
		{Name: "套餐A", Total: 100, Used: 0, Remain: 100,
			ExpireAt: exp, Key: "pkg-a|" + exp, Usable: true},
	}, nil
}
func (fakeUpstreamForLedger) ChatStream(*auth.Auth, []byte) (io.ReadCloser, int, []byte, error) {
	return nil, 0, nil, nil
}
func (fakeUpstreamForLedger) FetchModels(*auth.Auth) ([]provider.ModelInfo, error) { return nil, nil }
func (fakeUpstreamForLedger) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}
func (fakeUpstreamForLedger) Classify(int, string) provider.ErrKind       { return provider.ErrNone }
func (fakeUpstreamForLedger) Stream(http.ResponseWriter, io.Reader) error { return nil }
func (fakeUpstreamForLedger) Aggregate(io.Reader) (map[string]any, error) { return nil, nil }

// TestCheckinFeedsLedger —— 核心接线验证：
// 只移植账本包而没人喂数据，流水会恒空（此前就是这样）。
// 这里验证调度器签到后确实调用了 DiffCredits，账本因此产出条目。
func TestCheckinFeedsLedger(t *testing.T) {
	dir := t.TempDir()
	lg, err := ledger.New(dir)
	if err != nil {
		t.Fatalf("ledger.New: %v", err)
	}
	defer lg.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	s := New(Config{
		Pool:           p,
		Upstream:       fakeUpstreamForLedger{},
		Name:           "workbuddy",
		CheckinMinutes: []int{9 * 60},
		KeepaliveHours: []int{},
		Ledger:         lg,
	})

	r, err := s.CheckinAccount("u1")
	if err != nil {
		t.Fatalf("CheckinAccount: %v", err)
	}
	if !r.OK {
		t.Fatalf("签到应成功：%+v", r)
	}

	// 首次见此账号 → 存量额度记一条 earn（baseline）
	st := lg.Query(1, nil)
	if st == nil {
		t.Fatal("Query 不应返回 nil")
	}
	if st.Credit.Earn <= 0 {
		t.Errorf("账本应记录存量 earn，实际 earn=%d（说明 DiffCredits 没被调用）", st.Credit.Earn)
	}
	if len(st.Credit.Entries) == 0 {
		t.Errorf("账本应有条目，实际为空（说明没喂入）")
	}
}

// TestCheckinWithoutLedgerStillWorks —— Ledger 为 nil 时签到照常成功（旁路降级）。
func TestCheckinWithoutLedgerStillWorks(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	s := New(Config{
		Pool:           p,
		Upstream:       fakeUpstreamForLedger{},
		Name:           "workbuddy",
		CheckinMinutes: []int{9 * 60},
		KeepaliveHours: []int{},
		Ledger:         nil, // 旁路：不记账
	})
	r, err := s.CheckinAccount("u1")
	if err != nil {
		t.Fatalf("CheckinAccount: %v", err)
	}
	if !r.OK {
		t.Fatalf("Ledger 为 nil 时签到仍应成功：%+v", r)
	}
	if !r.HasRemain {
		t.Error("仍应查到余额")
	}
}

// TestLedgerDiffUsesExpireAt —— 到期归因：条目消失时应记为 expire 而非 spend。
// 这依赖 ResourceItem.ExpireAt 被正确传递（本次补的字段）。
func TestLedgerDiffUsesExpireAt(t *testing.T) {
	dir := t.TempDir()
	lg, err := ledger.New(dir)
	if err != nil {
		t.Fatalf("ledger.New: %v", err)
	}
	defer lg.Close()

	exp := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	items := []provider.ResourceItem{
		{Name: "套餐A", Total: 100, Used: 0, Remain: 100,
			ExpireAt: exp, Key: "pkg-a|" + exp, Usable: true},
	}
	// 首次：baseline earn
	lg.DiffCredits("workbuddy", "u1", 100, ledger.FromProviderItems(items))
	// 第二次：条目消失（模拟到期移除）→ 应记 expire
	lg.DiffCredits("workbuddy", "u1", 0, nil)
	st := lg.Query(1, nil)
	if st.Credit.Expire <= 0 {
		t.Errorf("条目消失应记为 expire，实际 expire=%d", st.Credit.Expire)
	}
}
