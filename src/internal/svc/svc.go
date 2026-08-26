// Package svc 组装多渠道运行时（pool + upstream + scheduler），供 serverd/ctl 共用。
package svc

import (
	"fmt"
	"path/filepath"
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

	return &Runtime{
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
	}, nil
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