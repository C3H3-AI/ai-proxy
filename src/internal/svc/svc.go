// Package svc 组装多渠道运行时（pool + upstream + scheduler），供 serverd/ctl 共用。
package svc

import (
	"encoding/json"
	"fmt"
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

	// 费率（模型积分倍率）缓存 —— 移植自上游 app.FeesInfo/RefreshPricing。
	pricingFP     string
	pricingMu     sync.RWMutex
	pricingCache  []provider.ModelPricing
	pricingFetched time.Time
	pricingErr    string
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
	for _, a := range wbAuths {
		wbPool.Add(a)
	}
	trPool := pool.New(filepath.Join(stateDir, "state-traework.json"))
	for _, a := range trAuths {
		trPool.Add(a)
	}
	qdPool := pool.New(filepath.Join(stateDir, "state-qoder.json"))
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

	wbSch := scheduler.New(scheduler.Config{Pool: wbPool, Upstream: wbUp, Name: "workbuddy", CheckinMinutes: minutes, KeepaliveHours: cfg.Schedule.KeepaliveHours})
	trSch := scheduler.New(scheduler.Config{Pool: trPool, Upstream: trUp, Name: "traework", CheckinMinutes: minutes, KeepaliveHours: cfg.Schedule.KeepaliveHours})
	// Qoder 无签到活动：调度器只做 token keepalive（每日 refresh 保活）
	qdSch := scheduler.New(scheduler.Config{Pool: qdPool, Upstream: qdUp, Name: "qoder", CheckinMinutes: nil, KeepaliveHours: cfg.Schedule.KeepaliveHours})

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
	}
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
		"note":       "费率随上游平台政策动态变化，请以官方为准；点击「刷新费率」重新拉取。",
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
			continue
		}
		if acct.NeedsRefresh(10 * time.Minute) {
			if err := rt.up.RefreshToken(acct); err != nil {
				errs = append(errs, rt.name+": token refresh failed")
				continue
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

// loadPricing 初始化费率缓存路径并载入磁盘缓存。
func (r *Runtime) loadPricing() {
	r.pricingFP = filepath.Join(filepath.Dir(r.Config.StateFile), "pricing-cache.json")
	r.loadPricingCache()
}
