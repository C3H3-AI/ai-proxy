// serverd.go — AI Proxy 无头服务入口（WorkBuddy + TraeWork 双平台）。
// 通过 svc 装配双平台运行时（pool + upstream + scheduler），暴露 OpenAI 兼容 HTTP。
// 模型名带 workbuddy/ 或 traework/ 前缀，自动路由到对应上游。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/config"
	"github.com/rockswang/workbuddy-wild/internal/provider"
	"github.com/rockswang/workbuddy-wild/internal/qoder"
	"github.com/rockswang/workbuddy-wild/internal/server"
	"github.com/rockswang/workbuddy-wild/internal/svc"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		if os.IsNotExist(err) ||
			(os.Getenv("WB2A_ALLOW_DEFAULT") == "1") {
			log.Printf("config %s not found or allow-default, using defaults+env", *cfgPath)
			if cfg, err = config.Load(""); err != nil {
				log.Fatalf("load config: %v", err)
			}
		} else {
			log.Fatalf("load config: %v", err)
		}
	}

	r, err := svc.New(cfg)
	if err != nil {
		log.Fatalf("svc: %v", err)
	}
	log.Printf("loaded accounts: workbuddy=%d (%s), traework=%d, qoder=%d from %s",
		len(r.WorkBuddyAccounts), cfg.Region, len(r.TraeWorkAccounts), len(r.QoderAccounts), cfg.AuthDir)

	runtimes := map[provider.Kind]*server.Runtime{
		provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: r.WorkBuddyPool, Upstream: r.WorkBuddyUpstream, StaticModels: server.WorkBuddyStaticModels()},
		provider.TraeWork:  {Kind: provider.TraeWork, Pool: r.TraeWorkPool, Upstream: r.TraeWorkUpstream, StaticModels: server.TraeWorkStaticModels()},
		provider.Qoder:     {Kind: provider.Qoder, Pool: r.QoderPool, Upstream: r.QoderUpstream, StaticModels: qoder.StaticModels()},
	}
	// auto 模型候选白名单
	autoModels := map[provider.Kind][]string{}
	if len(cfg.AutoModel.WorkBuddyModels) > 0 {
		autoModels[provider.WorkBuddy] = cfg.AutoModel.WorkBuddyModels
	}
	if len(cfg.AutoModel.TraeWorkModels) > 0 {
		autoModels[provider.TraeWork] = cfg.AutoModel.TraeWorkModels
	}
	if len(cfg.AutoModel.QoderModels) > 0 {
		autoModels[provider.Qoder] = cfg.AutoModel.QoderModels
	}

	h := server.NewHandler(server.Config{
		Runtimes:     runtimes,
		APIKey:       cfg.APIKey,
		Stats:        r.Stats,  // 可能为 nil（旁路降级），handler 已判空
		Ledger:       r.Ledger, // 同上
		HardCooldown: cfg.HardCreditDur,
		SoftCooldown: cfg.SoftRateDur,
		ErrThreshold: cfg.Cooldown.ErrThresh,
		ErrCooldown:  cfg.ErrCooldownDur,
		PricingFunc: func(kind provider.Kind) []provider.ModelPricing {
			return r.PricingForChannel(kind.String())
		},
		AutoModels: autoModels,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go r.WorkBuddyScheduler.Run(ctx)
	go r.TraeWorkScheduler.Run(ctx)
	go r.QoderScheduler.Run(ctx)

	// 费率/模型定时刷新：启动后先拉一次，之后每小时一次。
	// 此前只在打开面板「费率/模型」页时按需刷新（缓存为空或超过 1 小时），
	// 模型上下线、费率变动都无法及时反映到路由判定（模型是否免费 → 低积分账号能否使用）。
	go func() {
		refresh := func() {
			defer func() { _ = recover() }()
			r.RefreshPricing()
		}
		refresh()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}()

	// 费率接口：/api/fees 与 /api/fees/refresh 需要访问 svc.Runtime 的定价缓存，
	// 而 server.Handler 只接收 server.Config（不含 Runtime），因此在 serverd 层包装一层路由。
	fees := http.NewServeMux()
	fees.HandleFunc("GET /api/fees", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(r.FeesInfo())
	})
	fees.HandleFunc("POST /api/fees/refresh", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(r.FeesInfo())
		go func() {
			defer func() { _ = recover() }()
			r.RefreshPricing()
		}()
	})
	// 其余请求交给原 Handler（/v1/*、/status、/healthz）
	fees.Handle("/", h)

	srv := &http.Server{
		Addr:              cfg.Listen.Addr(),
		Handler:           fees,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	log.Printf("ai-proxy listening on %s (api_key=%v, workbuddy=%d traework=%d qoder=%d)", cfg.Listen.Addr(), cfg.APIKey != "", len(r.WorkBuddyAccounts), len(r.TraeWorkAccounts), len(r.QoderAccounts))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}
