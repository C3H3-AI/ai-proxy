// serverd.go — AI Proxy 无头服务入口（WorkBuddy + TraeWork 双平台）。
// 通过 svc 装配双平台运行时（pool + upstream + scheduler），暴露 OpenAI 兼容 HTTP。
// 模型名带 workbuddy/ 或 traework/ 前缀，自动路由到对应上游。
package main

import (
	"context"
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
	h := server.NewHandler(server.Config{
		Runtimes:     runtimes,
		APIKey:       cfg.APIKey,
		HardCooldown: cfg.HardCreditDur,
		SoftCooldown: cfg.SoftRateDur,
		ErrThreshold: cfg.Cooldown.ErrThresh,
		ErrCooldown:  cfg.ErrCooldownDur,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go r.WorkBuddyScheduler.Run(ctx)
	go r.TraeWorkScheduler.Run(ctx)
	go r.QoderScheduler.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen.Addr(),
		Handler:           h,
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