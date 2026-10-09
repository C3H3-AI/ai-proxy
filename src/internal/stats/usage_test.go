package stats

import (
	"strings"
	"testing"
	"time"
)

// AddUsage 异常负值帧钳 0 拒记；空模型名归 "unknown"。
func TestAddUsageClampsAndUnknownModel(t *testing.T) {
	s := newTestStats(t)
	now := time.Now()
	s.AddUsage("", -5, -10, -1.5, -100, -1, now)
	snap := s.Snapshot(now)
	if snap.Tokens != 0 || snap.CreditOutExact != 0 {
		t.Fatalf("负值帧应全钳 0: tokens=%d exact=%v", snap.Tokens, snap.CreditOutExact)
	}
	if len(snap.Models) != 1 || snap.Models[0].Model != "unknown" {
		t.Fatalf("空模型名应归 unknown: %+v", snap.Models)
	}
	if snap.Models[0].AvgTTFBMs != 0 || snap.Models[0].PerfSamples != 1 {
		t.Fatalf("TTFB 负值应钳 0 但仍计样本: %+v", snap.Models[0])
	}
	if snap.Models[0].AvgTokPerSec != 0 {
		t.Fatalf("负生成耗时不应计入吞吐: %+v", snap.Models[0])
	}
}

// 请求日志环形追加：容量封顶、Logs(limit) 取尾部（最新在末尾）。
func TestRequestLogRing(t *testing.T) {
	s := newTestStats(t)
	for i := 0; i < ringCap+10; i++ {
		s.AddRequestRow(ReqRow{Model: "m", Status: 200})
	}
	rows := s.Logs(15)
	if len(rows) != 15 {
		t.Fatalf("Logs(15) = %d", len(rows))
	}
	if rows[14].Seq != ringCap+10 {
		t.Fatalf("最新 seq = %d, want %d", rows[14].Seq, ringCap+10)
	}
	all := s.Logs(9999)
	if len(all) != ringCap {
		t.Fatalf("环形容量 = %d, want %d", len(all), ringCap)
	}
}

// Token 多天窗口：today/days7/days30/all 的归档边界与「已记录天数」。
func TestTokenUsageWindows(t *testing.T) {
	s := newTestStats(t)
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)
	old := base.AddDate(0, 0, -40)                   // 落在 30 日窗口外、计入 all
	s.AddUsage("workbuddy/m", 20, 10, 0, 0, 0, old)  // 30 token
	s.AddUsage("workbuddy/m", 30, 20, 0, 0, 0, base) // 50 token（今日=base）
	day2 := base.AddDate(0, 0, 1)
	s.AddUsage("workbuddy/m", 10, 10, 0, 0, 0, day2) // 20 token（跨天归档 base）

	snap := s.Snapshot(day2)
	tu := snap.TokenUsage
	if tu.Today != 20 {
		t.Fatalf("today = %d, want 20", tu.Today)
	}
	if tu.Days != 2 {
		t.Fatalf("days = %d, want 2（old+base 两个历史日）", tu.Days)
	}
	if tu.Days7 != 70 || tu.Days30 != 70 {
		t.Fatalf("days7/days30 = %d/%d, want 70/70（40 天前的 30 不在窗口）", tu.Days7, tu.Days30)
	}
	if tu.All != 100 {
		t.Fatalf("all = %d, want 100（70 + 窗口外 30）", tu.All)
	}
	if tu.ReqToday != 1 || tu.ReqAll != 3 {
		t.Fatalf("req today/all = %d/%d, want 1/3", tu.ReqToday, tu.ReqAll)
	}
}

// 模型性能均值：TTFB 取算术平均；吞吐按时长加权（生成 token ÷ 生成耗时）；
// 无生成耗时/无输出的请求只计入 TTFB；亚 50ms 伪影样本不计吞吐。
func TestModelPerfStats(t *testing.T) {
	s := newTestStats(t)
	now := time.Now()
	// m1：两次流式（TTFB 600/400 → 均值 500ms；吞吐 (100+300)/(2+3) = 80 tok/s）
	s.AddUsage("workbuddy/m1", 10, 100, 0.5, 600, 2, now)
	s.AddUsage("workbuddy/m1", 10, 300, 0.5, 400, 3, now)
	// m2：非流式无生成耗时（genSec=0）→ 只计 TTFB，不参与吞吐
	s.AddUsage("workbuddy/m2", 5, 0, 0.1, 1000, 0, now)

	snap := s.Snapshot(now)
	byName := map[string]ModelStat{}
	for _, m := range snap.Models {
		byName[m.Model] = m
	}
	m1 := byName["workbuddy/m1"]
	if m1.AvgTTFBMs != 500 || m1.PerfSamples != 2 {
		t.Fatalf("m1 平均 TTFB = %dms 样本=%d, want 500ms/2", m1.AvgTTFBMs, m1.PerfSamples)
	}
	if m1.AvgTokPerSec != 80 {
		t.Fatalf("m1 平均 tok/s = %v, want 80（按时长加权）", m1.AvgTokPerSec)
	}
	m2 := byName["workbuddy/m2"]
	if m2.AvgTTFBMs != 1000 || m2.PerfSamples != 1 {
		t.Fatalf("m2 平均 TTFB = %dms 样本=%d, want 1000ms/1", m2.AvgTTFBMs, m2.PerfSamples)
	}
	if m2.AvgTokPerSec != 0 {
		t.Fatalf("m2 无生成耗时不应有吞吐: %v", m2.AvgTokPerSec)
	}

	// 亚 50ms 伪影样本只计 TTFB，不计吞吐（防分母趋零抬高均值）
	s.AddUsage("workbuddy/m3", 10, 9000, 0, 20, 0.0007, now) // 亚阈值：若计入=1.29e7 tok/s
	s.AddUsage("workbuddy/m3", 10, 500, 0, 20, 1, now)       // 正常流式样本 500 tok/s
	for _, m := range s.Snapshot(now).Models {
		if m.Model != "workbuddy/m3" {
			continue
		}
		if m.AvgTTFBMs != 20 || m.PerfSamples != 2 {
			t.Fatalf("m3 TTFB/样本 = %dms/%d, want 20ms/2（伪影样本计 TTFB）", m.AvgTTFBMs, m.PerfSamples)
		}
		if m.AvgTokPerSec != 500 {
			t.Fatalf("m3 平均 tok/s = %v, want 500（亚 50ms 样本不计吞吐）", m.AvgTokPerSec)
		}
	}
	// 边界：恰好 50ms 计入
	s.AddUsage("workbuddy/m4", 10, 1000, 0, 100, 0.05, now)
	for _, m := range s.Snapshot(now).Models {
		if m.Model == "workbuddy/m4" && m.AvgTokPerSec != 20000 {
			t.Fatalf("m4 恰 50ms 应计入吞吐: %v", m.AvgTokPerSec)
		}
	}
}

// TraeWork 积分分摊：上游 usage 帧不下发积分，改用该渠道今日真实差值消耗按各模型
// token 占比摊到模型行——各行之和恒等于渠道真实消耗，且不影响 WorkBuddy 的精确值。
func TestTraeCreditAllocation(t *testing.T) {
	s := newTestStats(t)
	now := time.Now()
	s.ApplyPoll([]Account{
		{UID: "t1", Group: "traework", Credits: 1000},
		{UID: "w1", Group: "workbuddy", Credits: 1000},
	}, now)
	// tw 差值合计 300（200+100）；wb 差值 100（不应参与分摊）
	s.ApplyPoll([]Account{
		{UID: "t1", Group: "traework", Credits: 800},
		{UID: "w1", Group: "workbuddy", Credits: 900},
	}, now.Add(time.Minute))
	s.ApplyPoll([]Account{
		{UID: "t1", Group: "traework", Credits: 700},
		{UID: "w1", Group: "workbuddy", Credits: 900},
	}, now.Add(2*time.Minute))

	s.AddUsage("traework/a", 2000, 1000, 0, 0, 0, now) // 3000 token
	s.AddUsage("traework/b", 500, 500, 0, 0, 0, now)   // 1000 token
	s.AddUsage("workbuddy/m", 1000, 1000, 5, 0, 0, now)

	snap := s.Snapshot(now)
	byName := map[string]ModelStat{}
	var traeSum float64
	for _, m := range snap.Models {
		byName[m.Model] = m
		if strings.HasPrefix(m.Model, "traework/") {
			traeSum += m.Credit
		}
	}
	if got := byName["traework/a"].Credit; got != 225 {
		t.Fatalf("traework/a 分摊 = %v, want 225（300 × 3000/4000）", got)
	}
	if got := byName["traework/b"].Credit; got != 75 {
		t.Fatalf("traework/b 分摊 = %v, want 75", got)
	}
	if traeSum != 300 {
		t.Fatalf("TraeWork 分摊合计 = %v, want 300（= 渠道真实差值消耗）", traeSum)
	}
	if got := byName["workbuddy/m"].Credit; got != 5 {
		t.Fatalf("WorkBuddy 精确积分不应被分摊覆盖 = %v, want 5", got)
	}
}

// 无 TraeWork 差值消耗时不做分摊（保持 0，不伪造数值）。
func TestTraeCreditAllocationNoConsumption(t *testing.T) {
	s := newTestStats(t)
	now := time.Now()
	s.ApplyPoll([]Account{{UID: "t1", Group: "traework", Credits: 1000}}, now)
	s.AddUsage("traework/a", 10, 10, 0, 0, 0, now)
	for _, m := range s.Snapshot(now).Models {
		if m.Credit != 0 {
			t.Fatalf("%s 无差值消耗时积分应为 0，实际 %v", m.Model, m.Credit)
		}
	}
}

// 【fork 适配】原上游用例为「TraeCode 与 TraeWork 共用账号池，token 必须并入同一
// 分摊池」；本地 provider 无 TraeCode，该归并语义不存在。改为验证等价的分摊边界：
// **非 Trae 系渠道（qoder）的模型不参与 TraeWork 分摊池**——若误把它算进来，
// traework 各行之和会小于渠道真实差值消耗（300 被摊走一部分），口径不再闭合。
func TestTraeCreditAllocationExcludesOtherChannels(t *testing.T) {
	s := newTestStats(t)
	now := time.Now()
	s.ApplyPoll([]Account{
		{UID: "t1", Group: "traework", Credits: 1000},
		{UID: "w1", Group: "workbuddy", Credits: 1000},
		{UID: "q1", Group: "qoder", Credits: 1000},
	}, now)
	s.ApplyPoll([]Account{
		{UID: "t1", Group: "traework", Credits: 700},
		{UID: "w1", Group: "workbuddy", Credits: 900},
		{UID: "q1", Group: "qoder", Credits: 900},
	}, now.Add(time.Minute)) // tw 差值 300、qoder 差值 100

	s.AddUsage("traework/a", 2000, 1000, 0, 0, 0, now) // 3000 token
	s.AddUsage("qoder/c", 500, 500, 0, 0, 0, now)      // 1000 token → 不得进 trae 池
	s.AddUsage("workbuddy/m", 1000, 1000, 5, 0, 0, now)

	snap := s.Snapshot(now)
	byName := map[string]ModelStat{}
	var traeSum float64
	for _, m := range snap.Models {
		byName[m.Model] = m
		if strings.HasPrefix(m.Model, "traework/") {
			traeSum += m.Credit
		}
	}
	if got := byName["traework/a"].Credit; got != 300 {
		t.Fatalf("traework/a 分摊 = %v, want 300（池内仅此一模型，独得全部 300）", got)
	}
	if got := byName["qoder/c"].Credit; got != 0 {
		t.Fatalf("qoder/c 不应被计入 TraeWork 分摊池 = %v, want 0", got)
	}
	if traeSum != 300 {
		t.Fatalf("TraeWork 分摊合计 = %v, want 300（= 渠道真实差值消耗，口径闭合）", traeSum)
	}
	if got := byName["workbuddy/m"].Credit; got != 5 {
		t.Fatalf("WorkBuddy 精确积分不应被分摊覆盖 = %v, want 5", got)
	}
}

// 【fork 适配】原上游用例为「traecode 平台行须并入 traework」；本地无 TraeCode，
// 归并规则（normalizePlatform）退化为恒等映射。改为验证等价的归属边界：
// **三个本地渠道各自单列平台行、token 按前缀正确归属，互不串台**；
// 且白名单外渠道不产生幽灵平台行。
func TestPlatformRowsPerChannelTokens(t *testing.T) {
	s := newTestStats(t)
	now := time.Now()
	s.ApplyPoll([]Account{
		{UID: "t1", Group: "traework", Credits: 800},
		{UID: "w1", Group: "workbuddy", Credits: 500},
		{UID: "q1", Group: "qoder", Credits: 300},
	}, now)
	s.AddUsage("traework/a", 100, 100, 0, 0, 0, now) // 200 token
	s.AddUsage("qoder/c", 200, 200, 0, 0, 0, now)    // 400 token
	s.AddUsage("workbuddy/m", 50, 50, 0, 0, 0, now)  // 100 token
	s.AddUsage("YD/glm-5.3", 999, 999, 0, 0, 0, now) // 白名单外：不归属任何平台

	rows := s.Snapshot(now).Platforms
	byGroup := map[string]PlatformRow{}
	for _, r := range rows {
		byGroup[r.Group] = r
	}
	if _, ok := byGroup["YD"]; ok {
		t.Fatal("白名单外渠道不应单列平台行（未识别前缀不归属）")
	}
	if len(byGroup) != 3 {
		t.Fatalf("应恰有 3 个平台行（workbuddy/traework/qoder），实际 %+v", rows)
	}
	tw, ok := byGroup["traework"]
	if !ok {
		t.Fatal("缺少 traework 平台行")
	}
	if tw.Accounts != 1 || tw.Credits != 800 {
		t.Fatalf("traework 账号侧 = %+v，期望 accounts=1 credits=800", tw)
	}
	if tw.Tokens != 200 {
		t.Fatalf("traework Token = %d, want 200（不串入 qoder 的 token）", tw.Tokens)
	}
	if byGroup["qoder"].Tokens != 400 {
		t.Fatalf("qoder Token = %d, want 400", byGroup["qoder"].Tokens)
	}
	if wb := byGroup["workbuddy"]; wb.Tokens != 100 {
		t.Fatalf("workbuddy Token = %d, want 100", wb.Tokens)
	}
}

// channelOfModel：渠道前缀白名单（未识别前缀/裸模型名不归属，宁可少计不误计）。
func TestChannelOfModel(t *testing.T) {
	cases := []struct {
		model string
		want  string
		ok    bool
	}{
		{"workbuddy/deepseek-v4.1-flash", "workbuddy", true},
		{"traework/glm-5.3", "traework", true},
		{"qoder/claude-sonnet-4.5", "qoder", true},
		{"traecode/glm-5.3", "traecode", false}, // 【fork 适配】本地无 TraeCode：白名单外
		{"deepseek-v4.1-flash", "", false},      // 裸名（compat 映射，本地看不到渠道）
		{"YD/glm-5.3", "YD", false},             // 白名单外前缀：返回前缀但 ok=false（不归属）
		{"", "", false},
	}
	for _, c := range cases {
		g, ok := channelOfModel(c.model)
		if g != c.want || ok != c.ok {
			t.Errorf("channelOfModel(%q) = (%q,%v), want (%q,%v)", c.model, g, ok, c.want, c.ok)
		}
	}
}

// 模型行排序：积分降序 → token 降序 → 次数降序（分摊后排序，与显示值一致）。
func TestModelStatsSortedByCredit(t *testing.T) {
	s := newTestStats(t)
	now := time.Now()
	s.ApplyPoll([]Account{{UID: "w1", Group: "workbuddy", Credits: 1000}}, now)
	s.AddUsage("workbuddy/low", 100, 100, 1, 0, 0, now)
	s.AddUsage("workbuddy/high", 100, 100, 9, 0, 0, now)
	s.AddUsage("workbuddy/mid", 500, 500, 5, 0, 0, now)
	snap := s.Snapshot(now)
	if len(snap.Models) != 3 || snap.Models[0].Model != "workbuddy/high" ||
		snap.Models[1].Model != "workbuddy/mid" || snap.Models[2].Model != "workbuddy/low" {
		t.Fatalf("模型行应按积分降序: %+v", snap.Models)
	}
}
