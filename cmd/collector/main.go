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
		// ⚠️ 一期这个标志**不改变任何行为**（下面没有循环可跑）。留着是因为
		// verify/docker-compose.arm.yml 传了它，且 FR-116 的周期采集接上之后
		// 它就是"跑一轮后退出"的那个开关。删它要连那个 compose 一起改，
		// 换来的只是少两行。
		once    = flag.Bool("once", false, "跑一轮后退出（一期无周期采集，故当前无行为差异）")
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

	// ⚠️ 这行原先写的是"采集适配器尚未接入（#5/#6/#7）"，**已经不成立**：那三个
	// issue 早已完成，`internal/collector` 的适配器齐全且在跑真站点。不成立的是
	// **周期采集**（FR-116，P2 起）——一期只交付「按渠道手动触发立即刷新」
	// （FR-128 / AC-38），编排在 sla-core 的管理面里（POST /admin/channels/{id}/sync）。
	// 留着一句指向已关 issue 的话，会让人去翻三个已完成的 issue 找原因。
	logger.Info("collector 本轮无操作：一期无周期采集（FR-116 属 P2 起），" +
		"采集经管理面手动触发（FR-128，编排在 sla-core）")
}
