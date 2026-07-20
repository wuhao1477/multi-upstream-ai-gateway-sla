# ccLoad 源码评估

| 项目 | 内容 |
| --- | --- |
| 仓库 | `caidaoli/ccLoad` |
| 评估提交 | `94231ef997120a85e2a00d8e20896596759ad03c`（`master`） |
| 最近标签 | `v3.5.0`；评估提交无标签 |
| 许可证 | MIT |
| 结论 | 不进入正式短名单 |

## 1. 已验证能力

ccLoad 是清晰、轻量的多渠道转发器，适合解决基础可用性问题：

- 管理 API 可维护渠道、Key、模型、启停、优先级、冷却、日志、活跃请求和指标。
- 支持 RPM、并发、渠道/Key/URL/模型冷却和日成本上限。
- 请求按渠道、Key、URL 串行尝试；流式响应延迟提交，首字超时可在提交前换渠道。
- 客户端取消会传播到上游并关闭响应体。
- 记录首字、时长、成功率、Token、成本和活跃请求。

关键源码：

- [API 路由和管理接口](https://github.com/caidaoli/ccLoad/blob/94231ef997120a85e2a00d8e20896596759ad03c/internal/app/server.go#L812)
- [请求入口和候选选择](https://github.com/caidaoli/ccLoad/blob/94231ef997120a85e2a00d8e20896596759ad03c/internal/app/proxy_handler.go#L226)
- [渠道尝试循环](https://github.com/caidaoli/ccLoad/blob/94231ef997120a85e2a00d8e20896596759ad03c/internal/app/proxy_handler.go#L440)
- [上游请求和取消传播](https://github.com/caidaoli/ccLoad/blob/94231ef997120a85e2a00d8e20896596759ad03c/internal/app/proxy_forward.go#L1303)
- [Key、URL 和渠道回退](https://github.com/caidaoli/ccLoad/blob/94231ef997120a85e2a00d8e20896596759ad03c/internal/app/proxy_forward.go#L2152)
- [首字计时](https://github.com/caidaoli/ccLoad/blob/94231ef997120a85e2a00d8e20896596759ad03c/internal/app/request_context.go#L31)
- [渠道配置](https://github.com/caidaoli/ccLoad/blob/94231ef997120a85e2a00d8e20896596759ad03c/internal/model/config.go#L105)

## 2. 外部控制限制

渠道、Key 和冷却的修改可即时失效缓存，但系统设置更新会触发进程重启。项目没有服务端 Webhook、运行时插件或通用策略接口；协议通过编译期 `builtin.Register` 注册。

其“平滑加权轮询”的有效权重主要来自可用 Key 数，不是外部控制组件可直接设置的任意动态权重。定时渠道检查使用固定模型和固定测试内容，不是 PRD 定义的真实业务测活。

## 3. 与 PRD 的主要差距

- 只有本地模型价格、渠道成本倍率和日成本限额，没有上游价格版本、倍率可信确认、币种和账单核对。
- 没有账号实体、共享余额、Key 独立额度、在途费用预留和保守余额下限。
- 没有资源级能力目录和多层故障域。
- 健康数据以成功率和平均首字为主，没有按上下文和请求类型统计的 P50/P95/P99。
- 仅有 Codex 专用 Prompt Cache Hint，不具备通用会话亲和、缓存作用域和缓存损失预测。
- 没有真实业务测活预算、资格判断、冷却后再次试错和探索审计。
- 没有会话前缀 TTFT 账目、严格补偿/预算延续、并行接管及重复费用账本。
- 没有租户 SLA、错误预算、变更审计、策略版本和告警事件生命周期。

## 4. 不推荐原因

ccLoad 的请求链简单，首字前延迟提交和冷却机制值得参考，但外部 API 只能完成基础启停和优先级控制。要满足 PRD，需要在请求路由、状态模型、账务、测活、租户和审计等多个核心模块新增能力，整体会达到 L3。

**结论：不进入正式短名单。** 它适合轻量故障转移场景，不适合作为本项目的 SLA 数据面基础。
