// Command migrate 应用迁移与配置种子（`make migrate` 的入口）。
//
// 与 sla-core 启动时的 bootstrap 走**同一段代码**（internal/bootstrap.Run），
// 因此本命令能验证的正是生产启动路径，不存在"手动迁移能过、启动却失败"。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/bootstrap"
)

func main() {
	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"),
		"PG 连接串（默认取环境变量 DATABASE_URL）")
	timeout := flag.Duration("timeout", 2*time.Minute, "整体超时")
	flag.Parse()

	if *dsn == "" {
		fmt.Fprintln(os.Stderr,
			"缺少 DSN：用 -dsn 或设 DATABASE_URL，如 postgres://postgres:x@localhost:5432/sla")
		os.Exit(2)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	conn, err := pgx.Connect(ctx, *dsn)
	if err != nil {
		logger.Error("连接 PG 失败", "err", err)
		os.Exit(1)
	}
	defer func() { _ = conn.Close(ctx) }()

	if _, err := bootstrap.Run(ctx, conn, logger); err != nil {
		logger.Error("迁移失败", "err", err)
		os.Exit(1)
	}
	fmt.Println("✅ 迁移与种子完成")
}
