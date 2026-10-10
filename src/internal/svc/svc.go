// Package svc 组装多渠道运行时（pool + upstream + scheduler），供 serverd/ctl 共用。
package svc

import (
	"encoding/json"
	"fmt"
	"github.com/rockswang/workbuddy-wild/internal/ledger"
	"github.com/rockswang/workbuddy-wild/internal/stats"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/auth"
	"github.com/rockswang/workbuddy-wild/internal/config"
	"github.com/rockswang/workbuddy-wild/internal/pool"
	"github.com/rockswang/workbuddy-wild/internal/provider"
	"github.com/rockswang/workbuddy-wild/internal/qoder"
	"github.com/rockswang/workbuddy-wild/internal/scheduler"
	"github.com/rockswang/workbuddy-wild/internal/traework"
	"github.com/rockswang/workbuddy-wild/internal/upstream"
)

// Runtime 多渠道已装配好的运行时资源。
type Runtime struct {
	Config *config.Config

	WorkBuddyAccounts []*auth.Auth
	TraeWorkAccounts  []*auth.Auth
	QoderAccounts     []*auth.Auth

	WorkBuddyPool      *pool.Pool
	TraeWorkPool       *pool.Pool
	QoderPool          *pool.Pool
	WorkBuddyUpstream  provider.Upstream
	TraeWorkUpstream   provider.Upstream
	QoderUpstream      provider.Upstream
	WorkBuddyScheduler *scheduler.Scheduler
	TraeWorkScheduler  *scheduler.Scheduler
	QoderScheduler     *scheduler.Scheduler

	// Stats 运行统计引擎（内存实时 + 归档），Ledger 双流水账本。
	//
	// 两者刻意独立（上游 R43）：stats 是「今日」实时口径，
	// ledger 按 spend/expire 拆分的流水；差值含积分包到期作废，
	// 两套数字天然不同，不合并。
	//
	// 初始化失败时为 nil —— 统计是**旁路能力**，绝不能因此阻塞代理主流程。
	// 所有使用点必须做 nil 判断（见 handler.stats / handler.ledger）。
	Stats  *stats.Stats
	Ledger *ledger.Ledger

	// 费率（模型积分倍率）缓存 —— 移植自上游 app.FeesInfo/RefreshPricing。
	pricingFP      string
	pricingMu      sync.RWMutex
	pricingCache   []provider.ModelPricing
	pricingFetched time.Time
	pricingErr     string
}

// New 按配置装配多渠道运行时。
func New(cfg *config.Config) (*Runtime, error) {
	stateDir := filepath.Dir(cfg.StateFile)

	wbAuths, err := auth.LoadWorkBuddyDir(cfg.AuthDir, cfg.Region)
	if err != nil {
		return nil, fmt.Errorf("load workbuddy auths: %w", err)
	}
	trAuths, err := auth.LoadTraeDir(cfg.AuthDir)
	if err != nil {
		return nil, fmt.Errorf("load traework auths: %w", err)
	}
	qdAuths, err := auth.LoadQoderDir(cfg.AuthDir)
	if err != nil {
		return nil, fmt.Errorf("load qoder auths: %w", err)
	}

	wbPool := pool.New(filepath.Join(stateDir, "state-workbuddy.json"))
	wbPool.SetLowCredits(cfg.LowCreditThreshold)
	for _, a := range wbAuths {
		wbPool.Add(a)
	}
	trPool := pool.New(filepath.Join(stateDir, "state-traework.json"))
	trPool.SetLowCredits(cfg.LowCreditThreshold)
	for _, a := range trAuths {
		trPool.Add(a)
	}
	qdPool := pool.New(filepath.Join(stateDir, "state-qoder.json"))
	qdPool.SetLowCredits(cfg.LowCreditThreshold)
	for _, a := range qdAuths {
		qoder.EnsureFingerprint(a) // 老凭证补机器指纹
		qdPool.Add(a)
	}

	wbUp := upstream.New()
	wbUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	trUp := traework.New()
	trUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	qdUp := qoder.New()
	qdUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second

	minutes, err := config.ParseClockTimes(cfg.Schedule.CheckinTimes)
	if err != nil {
		return nil, fmt.Errorf("parse checkin times: %w", err)
	}

	// 账本先于调度器初始化：调度器签到后要把它拿到的权益明细喂给账本做差分记账。
	// 它是**旁路能力**：初始化失败只记日志、返回 nil，绝不阻塞代理主流程。
	lg := initLedger(dataDirOf(cfg))

	wbSch := scheduler.New(scheduler.Config{Pool: wbPool, Upstream: wbUp, Name: "workbuddy", CheckinMinutes: minutes, KeepaliveHours: cfg.Schedule.KeepaliveHours, Ledger: lg})
	trSch := scheduler.New(scheduler.Config{Pool: trPool, Upstream: trUp, Name: "traework", CheckinMinutes: minutes, KeepaliveHours: cfg.Schedule.KeepaliveHours, Ledger: lg})
	// Qoder 无签到活动：调度器只做 token keepalive（每日 refresh 保活）
	qdSch := scheduler.New(scheduler.Config{Pool: qdPool, Upstream: qdUp, Name: "qoder", CheckinMinutes: []int{}, KeepaliveHours: cfg.Schedule.KeepaliveHours, Ledger: lg})

	rt := &Runtime{
		Config:             cfg,
		WorkBuddyAccounts:  wbAuths,
		TraeWorkAccounts:   trAuths,
		QoderAccounts:      qdAuths,
		WorkBuddyPool:      wbPool,
		TraeWorkPool:       trPool,
		QoderPool:          qdPool,
		WorkBuddyUpstream:  wbUp,
		TraeWorkUpstream:   trUp,
		QoderUpstream:      qdUp,
		WorkBuddyScheduler: wbSch,
		TraeWorkScheduler:  trSch,
		QoderScheduler:     qdSch,
		Ledger:             lg,
	}
	rt.initStatsEngine() // 失败仅日志，不阻塞（旁路能力）
	rt.loadPricing()
	return rt, nil
}

// Upstream 返回平台对应的上游。
func (r *Runtime) Upstream(k provider.Kind) provider.Upstream {
	switch k {
	case provider.TraeWork:
		return r.TraeWorkUpstream
	case provider.Qoder:
		return r.QoderUpstream
	default:
		return r.WorkBuddyUpstream
	}
}

// Pool 返回平台对应的账号池。
func (r *Runtime) Pool(k provider.Kind) *pool.Pool {
	switch k {
	case provider.TraeWork:
		return r.TraeWorkPool
	case provider.Qoder:
		return r.QoderPool
	default:
		return r.WorkBuddyPool
	}
}

// Scheduler 返回平台对应的调度器。
func (r *Runtime) Scheduler(k provider.Kind) *scheduler.Scheduler {
	switch k {
	case provider.TraeWork:
		return r.TraeWorkScheduler
	case provider.Qoder:
		return r.QoderScheduler
	default:
		return r.WorkBuddyScheduler
	}
}

// Accounts 返回平台对应的账号列表。
func (r *Runtime) Accounts(k provider.Kind) []*auth.Auth {
	switch k {
	case provider.TraeWork:
		return r.TraeWorkAccounts
	case provider.Qoder:
		return r.QoderAccounts
	default:
		return r.WorkBuddyAccounts
	}
}

// FeesInfo 返回按渠道分组的费率信息（供 /api/fees）。
func (r *Runtime) FeesInfo() map[string]any {
	r.pricingMu.Lock()
	cached := r.pricingCache
	errMsg := r.pricingErr
	fetched := r.pricingFetched
	r.pricingMu.Unlock()

	if len(cached) == 0 {
		// 首次加载：异步拉取，先返回静态兜底
		go func() {
			defer func() { _ = recover() }()
			r.RefreshPricing()
		}()
		return staticFeesInfo()
	}

	// 缓存超过 1 小时，后台静默刷新
	if time.Since(fetched) > time.Hour {
		go func() {
			defer func() { _ = recover() }()
			r.RefreshPricing()
		}()
	}

	groups := map[string][]map[string]any{}
	for _, p := range cached {
		groups[p.Channel] = append(groups[p.Channel], map[string]any{
			"model": p.Model,
			"rate":  p.Rate,
			"note":  p.Note,
		})
	}

	channels := make([]map[string]any, 0)
	for _, ch := range []string{"workbuddy", "traework", "qoder"} {
		if models, ok := groups[ch]; ok {
			channels = append(channels, map[string]any{"channel": ch, "models": models})
		}
	}

	result := map[string]any{
		"note":       "费率随上游平台政策动态变化，请以官方为准；已开启每小时自动刷新，也可点击「刷新费率」立即拉取。",
		"channels":   channels,
		"disclaimer": "本工具仅聚合转发，不参与定价；渠道费率以各上游官方页面为准。",
		"cached_at":  fetched.Format("01-02 15:04"),
	}
	if errMsg != "" {
		result["error"] = errMsg
	}
	return result
}

type pricingRoute struct {
	name string
	pool *pool.Pool
	up   provider.Upstream
}

// RefreshPricing 逐个渠道拉取模型费率并落盘缓存。
func (r *Runtime) RefreshPricing() {
	routes := []pricingRoute{
		{"workbuddy", r.WorkBuddyPool, r.WorkBuddyUpstream},
		{"traework", r.TraeWorkPool, r.TraeWorkUpstream},
		{"qoder", r.QoderPool, r.QoderUpstream},
	}

	all := make([]provider.ModelPricing, 0)
	var errs []string
	for _, rt := range routes {
		if rt.pool == nil || rt.up == nil || len(rt.pool.List()) == 0 {
			continue
		}
		acct := rt.pool.Pick()
		if acct == nil {
			// Pick 排除低积分账号；若该渠道账号全是低积分，旧逻辑在这里 continue，
			// 该渠道的费率就永远拉不到 → 路由判定时费率未知被保守视为付费模型
			// → 低积分账号连免费模型都进不去（「0 积分免费的也不能用」）。
			// 回退到低积分账号：拉费率只是只读查询，不受低积分标记限制。
			acct = rt.pool.PickLowCredit()
		}
		if acct == nil {
			continue
		}
		if acct.NeedsRefresh(10 * time.Minute) {
			if err := rt.up.RefreshToken(acct); err != nil {
				errs = append(errs, rt.name+": token refresh failed")
				continue
			}
			// 上游 refresh 会轮换 refreshToken，必须落盘：否则只更新了内存，
			// 下次刷新仍用旧 refreshToken 会失败（定时刷新会让这个风险暴露得更频繁）。
			if err := acct.SaveAtomic(); err != nil {
				log.Printf("pricing refresh save failed platform=%s err=%v", rt.name, err)
			}
		}
		pricing, err := rt.up.FetchModelPricing(acct)
		if err != nil {
			errs = append(errs, rt.name+": "+shortErr(err))
			log.Printf("pricing fetch failed platform=%s err=%v", rt.name, err)
			continue
		}
		all = append(all, pricing...)
		log.Printf("pricing fetched platform=%s count=%d", rt.name, len(pricing))
	}

	sort.Slice(all, func(i, j int) bool {
		if all[i].Channel != all[j].Channel {
			return all[i].Channel < all[j].Channel
		}
		return all[i].Rate < all[j].Rate
	})

	r.pricingMu.Lock()
	r.pricingCache = all
	r.pricingFetched = time.Now()
	if len(errs) > 0 {
		r.pricingErr = strings.Join(errs, "; ")
	} else {
		r.pricingErr = ""
	}
	r.pricingMu.Unlock()
	r.savePricingCache()
}

func (r *Runtime) loadPricingCache() {
	if r.pricingFP == "" {
		return
	}
	raw, err := os.ReadFile(r.pricingFP)
	if err != nil {
		return
	}
	var cache struct {
		Models  []provider.ModelPricing `json:"models"`
		Fetched string                  `json:"fetched"`
	}
	if err := json.Unmarshal(raw, &cache); err != nil {
		return
	}
	r.pricingMu.Lock()
	r.pricingCache = cache.Models
	if cache.Fetched != "" {
		if t, err := time.Parse(time.RFC3339, cache.Fetched); err == nil {
			r.pricingFetched = t
		}
	}
	r.pricingMu.Unlock()
}

func (r *Runtime) savePricingCache() {
	if r.pricingFP == "" {
		return
	}
	r.pricingMu.Lock()
	cache := struct {
		Models  []provider.ModelPricing `json:"models"`
		Fetched string                  `json:"fetched"`
	}{Models: r.pricingCache, Fetched: r.pricingFetched.Format(time.RFC3339)}
	r.pricingMu.Unlock()
	raw, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(r.pricingFP, raw, 0o644)
}

// staticFeesInfo 无账号/首次加载时的静态兜底说明。
func staticFeesInfo() map[string]any {
	return map[string]any{
		"note": "费率随上游平台政策动态变化，请以官方为准；添加账号后点击「刷新费率」拉取实时数据。",
		"channels": []map[string]any{
			{"channel": "workbuddy", "models": []map[string]any{
				{"model": "auto", "rate": 0, "note": "自动路由最优模型"},
				{"model": "deepseek-v4-*", "rate": 0, "note": "按对话计费"},
				{"model": "glm-*", "rate": 0, "note": "按对话计费"},
			}},
			{"channel": "traework", "models": []map[string]any{
				{"model": "DeepSeek-*", "rate": 0, "note": "按 token 计费"},
				{"model": "glm-5*", "rate": 0, "note": "按 token 计费"},
			}},
			{"channel": "qoder", "models": []map[string]any{
				{"model": "deepseek-v4-*", "rate": 0, "note": "按积分计费"},
				{"model": "qwen3.* / glm-5.*", "rate": 0, "note": "按积分计费"},
			}},
		},
		"disclaimer": "本工具仅聚合转发，不参与定价；渠道费率以各上游官方页面为准。",
	}
}

func shortErr(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	if len(msg) > 160 {
		msg = msg[:160]
	}
	return msg
}

// PricingForChannel 返回指定渠道的模型定价列表（只读快照）。
// 用于 auto 模型选择费率最低的模型。
func (r *Runtime) PricingForChannel(channel string) []provider.ModelPricing {
	r.pricingMu.RLock()
	cached := r.pricingCache
	r.pricingMu.RUnlock()
	if len(cached) == 0 {
		return nil
	}
	out := make([]provider.ModelPricing, 0, len(cached))
	for _, p := range cached {
		if p.Channel == channel {
			out = append(out, p)
		}
	}
	return out
}

// loadPricing 初始化费率缓存路径并载入磁盘缓存。
func (r *Runtime) loadPricing() {
	r.pricingFP = filepath.Join(filepath.Dir(r.Config.StateFile), "pricing-cache.json")
	r.loadPricingCache()
}

// initStatsEngine 初始化运行统计引擎。
//
// 统计是**旁路能力**：初始化失败只记日志并把字段留空（nil），
// 绝不返回 error 阻塞代理主流程。消费点（handler.statsSnapshot）
// 必须先做 nil 判断。
func (r *Runtime) initStatsEngine() {
	logf := func(format string, args ...any) { log.Printf(format, args...) }
	st, err := stats.New(dataDirOf(r.Config), logf)
	if err != nil {
		log.Printf("stats init failed (旁路降级，代理不受影响): %v", err)
		return
	}
	r.Stats = st
	// 每 60s 喂一次账号快照差值；进程内已有 5s 落盘。
	go r.statsPollLoop()
}

// initLedger 初始化双流水账本（旁路能力，失败返回 nil）。
//
// 与 stats 刻意独立（上游 R43）：stats 是「今日」实时口径，
// ledger 按 spend/expire 拆分流水；差值含积分包到期作废，两套数字天然不同。
func initLedger(dir string) *ledger.Ledger {
	if dir == "" {
		dir = "data"
	}
	lg, err := ledger.New(filepath.Join(dir, "ledger"))
	if err != nil {
		log.Printf("ledger init failed (旁路降级，代理不受影响): %v", err)
		return nil
	}
	return lg
}

// dataDirOf 返回状态文件所在目录（stats/ledger 落盘位置）。
func dataDirOf(cfg *config.Config) string {
	dir := filepath.Dir(cfg.StateFile)
	if dir == "" || dir == "." {
		return "data"
	}
	return dir
}

// statsPollLoop 周期把各渠道账号快照喂给统计引擎。
func (r *Runtime) statsPollLoop() {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for range t.C {
		r.ApplyStatsPoll()
	}
}

// ApplyStatsPoll 聚合三个渠道的账号状态喂给统计引擎；Stats 为 nil 时安全返回。
func (r *Runtime) ApplyStatsPoll() {
	if r.Stats == nil {
		return
	}
	now := time.Now()
	var list []stats.Account
	for _, k := range []provider.Kind{provider.WorkBuddy, provider.TraeWork, provider.Qoder} {
		p := r.Pool(k)
		if p == nil {
			continue
		}
		for _, s := range p.List() {
			until := ""
			if !s.Until.IsZero() {
				until = s.Until.Format(time.RFC3339) // 字典序即时间序
			}
			list = append(list, stats.Account{
				UID:      s.UID,
				Group:    k.String(),
				Nickname: s.Nickname,
				Credits:  s.Credits,
				Cooling:  s.Cooling,
				Disabled: s.Disabled,
				Reason:   s.Reason,
				Until:    until,
				ErrCount: s.ErrCount,
			})
		}
	}
	if len(list) == 0 {
		return // 空切片会跳过本轮基线，不调用以免清空
	}
	r.Stats.ApplyPoll(list, now)
}
