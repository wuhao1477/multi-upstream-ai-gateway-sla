// Command collector 是采集器入口（旁路控制路径）。
//
// 06 开放点 3 已定：一期同二进制子命令即可，部署简单；量级上来再拆独立容器。
// 这里保持独立 main 以便 compose 单独起一个 collector 服务（06 §1 拓扑），
// 二者共用 internal/collector 的实现。
//
// 采集器是**异步旁路**：任何抖动不阻塞同步决策路径（01 §5）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla-public/internal/bootstrap"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla-public/internal/collection"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla-public/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla-public/internal/store"
)

var version = "dev"

func main() {
	var (
		once    = flag.Bool("once", false, "跑一轮后退出")
		dsn     = flag.String("dsn", os.Getenv("DATABASE_URL"), "PG 连接串")
		showVer = flag.Bool("version", false, "打印版本后退出")
	)
	flag.Parse()

	if *showVer {
		fmt.Println(version)
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(*dsn, *once, logger); err != nil {
		logger.Error("collector 退出", "err", err)
		os.Exit(1)
	}
}

func run(dsn string, once bool, logger *slog.Logger) error {
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

	conn, release, err := pool.Acquire(startCtx)
	if err != nil {
		return err
	}
	boot, err := bootstrap.Run(startCtx, conn, logger)
	release()
	if err != nil {
		return fmt.Errorf("初始化: %w", err)
	}
	snap := boot.Snapshot
	requestInterval, err := snap.Int("collector_request_interval_ms")
	if err != nil {
		return err
	}
	periods, err := collection.IntervalsFromSnapshot(snap)
	if err != nil {
		return err
	}
	httpClient := collector.NewClient(time.Duration(requestInterval) * time.Millisecond)
	runner := collection.NewRunner(pool, httpClient)
	service := collection.NewService(pool, runner, collection.NewSchedule(periods), logger)

	logger.Info("collector 启动",
		"version", version, "phase", "P1", "once", once,
		"request_interval_ms", requestInterval,
		"balance_interval", periods.Balance,
		"keyquota_interval", periods.KeyQuota,
		"price_interval", periods.Price,
		"catalog_interval", periods.Catalog)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if once {
		return service.RunOnce(ctx)
	}
	if err := service.Run(ctx); errors.Is(err, context.Canceled) {
		return nil
	} else {
		return err
	}
}
