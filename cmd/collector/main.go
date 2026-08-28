// Command collector 是采集器入口（旁路控制路径）。
//
// 06 开放点 3 已定：一期同二进制子命令即可，部署简单；量级上来再拆独立容器。
// 这里保持独立 main 以便 compose 单独起一个 collector 服务（06 §1 拓扑），
// 二者共用 internal/collector 的实现。
//
// 采集器是**异步旁路**：任何抖动不阻塞同步决策路径（01 §5）。
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/config"
)

var version = "dev"

func main() {
	var (
		once    = flag.Bool("once", false, "跑一轮后退出（供 CI 与手动排查用）")
		showVer = flag.Bool("version", false, "打印版本后退出")
	)
	flag.Parse()

	if *showVer {
		fmt.Println(version)
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	snap := config.NewSnapshot(nil)
	if err := snap.Validate(); err != nil {
		logger.Error("配置校验失败", "err", err)
		os.Exit(1)
	}

	interval, err := snap.Int("collector_request_interval_ms")
	if err != nil {
		logger.Error("读取采集间隔失败", "err", err)
		os.Exit(1)
	}

	logger.Info("collector 启动",
		"version", version, "phase", "P1", "once", *once,
		"request_interval_ms", interval)

	// 采集实现随 #5（Detect+Auth）、#6（Groups/Keys）、#7（Pricing/Catalog）接入。
	// 骨架阶段只验证配置可读 —— 这已经能挡住"键名写错"这类问题。
	logger.Info("采集适配器尚未接入（#5/#6/#7），本轮无操作")
}
