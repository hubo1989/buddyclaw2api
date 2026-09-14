// main.go workbuddy2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/autoclaw"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/redisstore"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		// 配置文件不存在时给一次机会用纯默认 + env。
		// 必须用 errors.Is 解包——Load 内部用 %w 包装了 os 错误，
		// os.IsNotExist 不解包，会让本分支永远不成立（死代码）。
		if errors.Is(err, os.ErrNotExist) {
			log.Printf("config %s not found, using defaults+env", *cfgPath)
			cfg, err = Load("")
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	// 安全闸门：空 key + 非回环监听直接拒绝启动，避免无鉴权暴露。
	if err := cfg.ValidateForServe(); err != nil {
		log.Fatalf("unsafe config: %v", err)
	}

	auths, err := auth.LoadDir(cfg.AuthDir, cfg.Region)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d %s account(s) from %s", len(auths), cfg.Region, cfg.AuthDir)

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	defer p.Flush() // 进程退出前强制落盘（后台 flush 每 5s 一次，退出时补一次）
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// 熔断器 + 在途上限 + 三因子加权调优（从 config 注入，非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	// autoclaw 上游（默认关闭；启用时从 auth 目录加载 autoclaw-*.json）。
	var acSub *autoclaw.Subsystem
	adminAuthDir := ""
	if cfg.Autoclaw.Enabled {
		acDir := cfg.Autoclaw.AuthDir
		if acDir == "" {
			acDir = cfg.AuthDir
		}
		adminAuthDir = acDir
		acCreds, err := autoclaw.LoadDir(acDir)
		if err != nil {
			log.Fatalf("load autoclaw auths: %v", err)
		}
		if len(acCreds) == 0 {
			log.Printf("WARN: autoclaw.enabled 但 %s 下无 autoclaw-*.json — "+
				"先运行 wb2api-autoclaw-login -phone <手机号> 登录（账号须服务独占，勿与桌面端同号）", acDir)
		} else {
			for _, c := range acCreds {
				log.Printf("autoclaw account: uid=%s phone=%s", c.UID, autoclaw.MaskPhoneExport(c.Phone))
			}
		}
		acSub = autoclaw.NewSubsystem(autoclaw.SubsystemConfig{
			Host:             cfg.Autoclaw.Host,
			Timeout:          time.Duration(cfg.Autoclaw.TimeoutSeconds) * time.Second,
			RefreshSkew:      cfg.AutoclawRefreshSkew,
			SoftCooldown:     cfg.AutoclawSoftDur,
			BreakerThreshold: cfg.Autoclaw.BreakerThreshold,
			BreakerCooldown:  cfg.AutoclawBreakerDur,
			BreakerMax:       cfg.AutoclawBreakerMax,
		}, acCreds)
		log.Printf("autoclaw upstream enabled: %d account(s)", acSub.Count())
	}

	up := upstream.New()
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	up.SanitizeFingerprints = cfg.Features.SanitizeBlacklistFingerprints

	sch := scheduler.New(scheduler.Config{
		Pool:           p,
		Upstream:       up,
		CheckinHours:   cfg.Schedule.CheckinHours,
		KeepaliveHours: cfg.Schedule.KeepaliveHours,
		Autoclaw:       acSub,
	})

	h := server.NewHandler(server.Config{
		Pool:          p,
		Upstream:      up,
		APIKey:        cfg.APIKey,
		Session:       sessRouter,
		StickyCount:   sessCount,
		RedisMode:     redisMode,
		SoftCooldown:  cfg.SoftRateDur,
		Autoclaw:      acSub,
		EnableAdmin:   acSub != nil,
		AdminAuthDir:  adminAuthDir,
		AdminHost:     cfg.Autoclaw.Host,
		AdminPanelURL: os.Getenv("WB2A_ADMIN_PANEL_URL"),
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		p.Flush() // 信号触发：先落盘再做优雅停机
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("workbuddy2api listening on %s (api_key=%v)", cfg.Listen, cfg.APIKey != "")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}
