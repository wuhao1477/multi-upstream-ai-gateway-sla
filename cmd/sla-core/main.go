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
	"syscall"
	"time"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/config"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/health"
)

// version 由构建时注入（-ldflags "-X main.version=..."）。
var version = "dev"

func main() {
	var (
		addr    = flag.String("addr", envOr("SLA_ADDR", ":8080"), "监听地址（管理平面 + /healthz）")
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

	if err := run(*addr, logger); err != nil {
		logger.Error("退出", "err", err)
		os.Exit(1)
	}
}

func run(addr string, logger *slog.Logger) error {
	// P1 骨架阶段：配置快照用文档默认值构建（库读取随 #2/#4 接入）。
	// 之所以现在就构建并校验：Validate 把"某键被改成非法值"暴露在启动阶段，
	// 而不是等采集器凌晨跑到那一行才失败（config.Validate 的注释）。
	snap := config.NewSnapshot(nil)
	if err := snap.Validate(); err != nil {
		return fmt.Errorf("配置校验失败: %w", err)
	}
	logger.Info("配置快照已加载", "keys", len(config.Keys()))

	mux := http.NewServeMux()
	mux.Handle("/healthz", &health.Handler{
		// TODO(#2): 接入真实 PG 连接池后替换。
		// 当前返回 nil 表示"可达"，仅用于骨架自检；#2 会换成 pgxpool。
		DB:          stubPinger{},
		HasSnapshot: func() bool { return snap != nil },
	})

	srv := &http.Server{
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
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
		return srv.Shutdown(shutdownCtx)
	}
}

// stubPinger 是 #2 接入 PG 之前的占位。
// 刻意不写成"永远健康"的空实现放在 health 包里 —— 那会让生产代码带上
// 一个可能被误用的假探针。放在 main 里，接入时删掉即可。
type stubPinger struct{}

func (stubPinger) Ping(context.Context) error { return nil }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
