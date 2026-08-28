// Package bootstrap 是启动初始化：选主 → 迁移 → 种子 → 构建配置快照。
//
// 幂等（06 §2.2）：每次启动都跑，已完成的步骤自动跳过。
// 选主用 PG 咨询锁，避免双 core 实例重复迁移。
package bootstrap

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/config"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// Result 是初始化产物。
type Result struct {
	Snapshot *config.Snapshot
}

// Run 执行完整初始化。
//
// 顺序不可交换：
//  1. 迁移（建表）—— 后面两步都依赖表存在
//  2. 种子（灌 72 键）—— 快照要读它
//  3. 快照（读配置）—— 决策路径只读快照（01 §5）
func Run(ctx context.Context, conn *pgx.Conn, logger *slog.Logger) (*Result, error) {
	if logger == nil {
		logger = slog.Default()
	}
	start := time.Now()

	if err := store.Migrate(ctx, conn, logger); err != nil {
		return nil, fmt.Errorf("迁移: %w", err)
	}

	if err := store.SeedConfigParams(ctx, conn, logger); err != nil {
		return nil, fmt.Errorf("配置种子: %w", err)
	}

	values, err := store.LoadConfigValues(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("读配置: %w", err)
	}
	snap := config.NewSnapshot(values)

	// 启动即校验：把"某键被改成非法值"暴露在启动阶段，
	// 而不是等采集器凌晨跑到那一行才失败。
	if err := snap.Validate(); err != nil {
		return nil, fmt.Errorf("配置校验: %w", err)
	}

	logger.Info("初始化完成",
		"elapsed_ms", time.Since(start).Milliseconds(),
		"config_keys", len(values))
	return &Result{Snapshot: snap}, nil
}
