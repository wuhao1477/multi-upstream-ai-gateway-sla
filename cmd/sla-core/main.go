// Command sla-core 是主服务入口（数据平面 + 管理平面同进程）。
//
// 当前交付阶段 P1（PRD §2.1.0）：**只有管理平面**。
// 数据平面 /v1/* 属 P2，本进程此阶段不监听它 —— 这是范围而非缺失
// （ISSUE-005 §2 的"不做"清单）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/admin"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/bootstrap"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/config"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/health"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// version 由构建时注入（-ldflags "-X main.version=..."）。
var version = "dev"

func main() {
	var (
		addr    = flag.String("addr", envOr("SLA_ADDR", ":8080"), "监听地址（管理平面 + /healthz）")
		dsn     = flag.String("dsn", os.Getenv("DATABASE_URL"), "PG 连接串")
		showVer = flag.Bool("version", false, "打印版本后退出")
	)
	flag.Parse()

	if *showVer {
		fmt.Println(version)
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	if err := run(*addr, *dsn, logger); err != nil {
		logger.Error("退出", "err", err)
		os.Exit(1)
	}
}

func run(addr, dsn string, logger *slog.Logger) error {
	if dsn == "" {
		return errors.New("缺少 DSN：设 DATABASE_URL 或用 -dsn")
	}

	startCtx, cancelStart := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelStart()

	pool, err := store.NewPool(startCtx, dsn)
	if err != nil {
		return fmt.Errorf("连接 PG: %w", err)
	}
	defer pool.Close()

	// bootstrap 与 cmd/migrate 走同一段代码：手动迁移能过、启动却失败的
	// 情形因此不存在（cmd/migrate 的注释）。
	conn, release, err := pool.Acquire(startCtx)
	if err != nil {
		return err
	}
	res, err := bootstrap.Run(startCtx, conn, logger)
	release()
	if err != nil {
		return fmt.Errorf("初始化: %w", err)
	}

	// 快照用 atomic 持有：apply 配置后需原地替换，而决策路径只读它（01 §5）。
	var snap atomic.Pointer[config.Snapshot]
	snap.Store(res.Snapshot)

	// 配置变更后重建快照，使新值对决策路径生效（09 §2 末条）。
	// 失败只记日志不中断服务：旧快照仍然可用，比"因为读不到新配置而挂掉"好。
	rebuild := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c, rel, err := pool.Acquire(ctx)
		if err != nil {
			logger.Error("快照重建取连接失败", "err", err)
			return
		}
		defer rel()
		values, err := store.LoadConfigValues(ctx, c)
		if err != nil {
			logger.Error("快照重建读配置失败", "err", err)
			return
		}
		ns := config.NewSnapshot(values)
		if err := ns.Validate(); err != nil {
			logger.Error("新快照校验失败，保留旧快照", "err", err)
			return
		}
		snap.Store(ns)
		logger.Info("配置快照已重建")
	}

	adminToken := os.Getenv("ADMIN_TOKEN")
	if adminToken == "" {
		// 不 fatal：/healthz 仍应可服务（LB 需要它）。管理平面自己会返 503。
		logger.Warn("ADMIN_TOKEN 未设置，管理平面将拒绝全部请求")
	}

	// 采集器：三家族适配器 + 落库 sink（09 §5.0bis 的编排在 collector.Syncer）
	interval := 200 * time.Millisecond
	if v, err := snap.Load().Int("collector_request_interval_ms"); err == nil && v > 0 {
		interval = time.Duration(v) * time.Millisecond
	}
	hc := collector.NewClient(interval)
	sink := store.NewCollectorSink(pool)
	auth := collector.NewAuthenticator(store.NewCredentialStore(pool))

	mux := http.NewServeMux()
	mux.Handle("/healthz", &health.Handler{
		DB:          pool,
		HasSnapshot: func() bool { return snap.Load() != nil },
	})

	srv := admin.NewServer(pool, adminToken, logger, rebuild)
	srv.Snapshot = func() *config.Snapshot { return snap.Load() }
	srv.Detect = func(ctx context.Context, baseURL string) (collector.DetectResult, error) {
		return collector.Detect(ctx, hc.HC, baseURL)
	}
	srv.Sync = func(ctx context.Context, ch store.Channel) (*collector.SyncResult, error) {
		return runChannelSync(ctx, pool, hc, sink, auth, ch)
	}
	credStore := store.NewCredentialStore(pool)
	srv.SaveCredential = credStore.Save
	srv.SaveDetected = credStore.SaveDetected
	srv.Routes(mux)
	srv.UpstreamRoutes(mux)
	srv.ImportRoutes(mux)
	srv.WebRoutes(mux)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 优雅关闭：SIGTERM 后先让 Caddy 摘除本实例，再排空在途请求。
	// 这对 AC-27（停一个实例、请求全成功）是必要的 —— 硬杀会让在途请求失败。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("sla-core 启动", "addr", addr, "version", version, "phase", "P1")
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("收到关闭信号，开始优雅关闭")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
