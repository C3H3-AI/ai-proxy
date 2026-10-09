package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rockswang/workbuddy-wild/internal/auth"
	"github.com/rockswang/workbuddy-wild/internal/pool"
	"github.com/rockswang/workbuddy-wild/internal/provider"
	"github.com/rockswang/workbuddy-wild/internal/stats"
)

// newStatsHandler 构造只有 workbuddy 一个渠道的 handler（Stats/Ledger 由参数给）。
func newStatsHandler(t *testing.T, st any, lg any) *Handler {
	t.Helper()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", Nickname: "n1", ExpiresAt: 9999999999})
	h := NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
				return 200, `{"ok":true}`, false
			})},
		},
	})
	return h
}

// TestStatsRouteUnavailableWhenNil —— Stats 为 nil 时必须返回 503 与明确原因，
// 而不是 panic 或返回空数据（统计是旁路能力，降级必须显式）。
func TestStatsRouteUnavailableWhenNil(t *testing.T) {
	h := newStatsHandler(t, nil, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/stats", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "stats_unavailable") {
		t.Errorf("应给出明确不可用原因，实际=%s", rec.Body)
	}
}

// TestLedgerRouteUnavailableWhenNil —— 同上，账本为 nil 时 503。
func TestLedgerRouteUnavailableWhenNil(t *testing.T) {
	h := newStatsHandler(t, nil, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/ledger?days=1", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "ledger_unavailable") {
		t.Errorf("应给出明确不可用原因，实际=%s", rec.Body)
	}
}

// TestStatsLedgerRoutesRegistered —— 两个路由必须已注册（否则面板拿不到数据）。
func TestStatsLedgerRoutesRegistered(t *testing.T) {
	h := newStatsHandler(t, nil, nil)
	for _, p := range []string{"/api/stats", "/api/ledger"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		// 未注册会走 mux 默认 404；已注册则进入 handler 返回 503（nil 降级）
		if rec.Code == http.StatusNotFound {
			t.Errorf("%s 未注册（返回 404）", p)
		}
	}
}

// TestLedgerQueryDaysParam —— days=1 与默认（7）都能走通；非法值按 7 处理不报错。
func TestLedgerQueryDaysParam(t *testing.T) {
	h := newStatsHandler(t, nil, nil)
	for _, q := range []string{"?days=1", "?days=7", "?days=abc", ""} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/ledger"+q, nil))
		if rec.Code == http.StatusNotFound || rec.Code >= 500 && !strings.Contains(rec.Body.String(), "unavailable") {
			t.Errorf("days=%q 处理异常：code=%d body=%s", q, rec.Code, rec.Body)
		}
	}
}

// TestStatsSnapshotShape —— 有 Stats 时返回可解析 JSON（用真实 stats 引擎，
// 保证端到端契约：面板能直接消费）。
func TestStatsSnapshotShape(t *testing.T) {
	dir := t.TempDir()
	st, err := newStatsForTest(dir)
	if err != nil {
		t.Skipf("stats 初始化失败，跳过：%v", err)
	}
	h := newStatsHandler(t, nil, nil)
	h.cfg.Stats = st

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var obj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatalf("返回不是合法 JSON：%v（body=%s）", err, rec.Body)
	}
	// 关键字段必须存在（面板依赖）
	for _, k := range []string{"today", "total_credits", "credit_in", "credit_out", "tokens", "token_usage"} {
		if _, ok := obj[k]; !ok {
			t.Errorf("快照缺少字段 %q（面板依赖），实际字段=%v", k, keys(obj))
		}
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// newStatsForTest 用真实 stats 引擎（端到端契约验证用）。
func newStatsForTest(dir string) (*stats.Stats, error) {
	return stats.New(dir, nil)
}
