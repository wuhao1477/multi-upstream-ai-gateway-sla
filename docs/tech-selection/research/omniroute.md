# OmniRoute 源码评估

| 项目 | 内容 |
| --- | --- |
| 仓库 | `diegosouzapw/OmniRoute` |
| 评估提交 | `3ba3cd145eefafcb22efa16d6110daeb65c8df06`（`release/v3.8.49`） |
| 最近标签 | `v3.8.48`；评估提交无标签 |
| 许可证 | MIT |
| 结论 | 条件短名单，但不作为最终基础 |

## 1. 已验证能力

OmniRoute 不是单纯桌面客户端。它可以只构建后端，并具有本轮候选中最完整的正式插件、Middleware 和 Webhook 能力：

- Provider、Connection、Key、Combo 均有带管理鉴权的 API，可动态启停、调整优先级、并发和额度阈值。
- 服务端插件支持请求、响应、错误、模型选择、Combo 解析、限流、额度、Provider 错误和流开始/结束等 Hook。
- Webhook 包含请求成功/失败、Provider 错误/恢复、额度耗尽和 Combo 切换等七类事件。
- 有会话亲和、Connection Pin、额度预检、熔断、缓存 Token、TTFT、完整时长、输出速度、费用和 Combo 尝试记录。
- 流在首个非心跳 SSE 事件前不会提交；超时会取消读取，再由 Combo 顺序尝试下一个目标。

关键源码：

- [Chat API 入口](https://github.com/diegosouzapw/OmniRoute/blob/3ba3cd145eefafcb22efa16d6110daeb65c8df06/src/app/api/v1/chat/completions/route.ts#L47)
- [请求前 Middleware](https://github.com/diegosouzapw/OmniRoute/blob/3ba3cd145eefafcb22efa16d6110daeb65c8df06/src/sse/handlers/chat.ts#L520)
- [候选、亲和、额度和熔断](https://github.com/diegosouzapw/OmniRoute/blob/3ba3cd145eefafcb22efa16d6110daeb65c8df06/src/sse/handlers/chat.ts#L1086)
- [首事件就绪判断](https://github.com/diegosouzapw/OmniRoute/blob/3ba3cd145eefafcb22efa16d6110daeb65c8df06/open-sse/utils/streamReadiness.ts#L252)
- [Combo 顺序回退](https://github.com/diegosouzapw/OmniRoute/blob/3ba3cd145eefafcb22efa16d6110daeb65c8df06/open-sse/services/combo.ts#L1784)
- [插件 Hook](https://github.com/diegosouzapw/OmniRoute/blob/3ba3cd145eefafcb22efa16d6110daeb65c8df06/src/lib/plugins/hooks.ts#L35)
- [Webhook 事件](https://github.com/diegosouzapw/OmniRoute/blob/3ba3cd145eefafcb22efa16d6110daeb65c8df06/src/lib/webhooks/eventDescriptions.ts#L1)
- [用量与 TTFT](https://github.com/diegosouzapw/OmniRoute/blob/3ba3cd145eefafcb22efa16d6110daeb65c8df06/src/lib/usage/usageHistory.ts#L675)

## 2. 不能直接满足的核心需求

OmniRoute 的价格、额度和 Combo 是可复用基础，但缺少 PRD 所要求的业务语义：

- 没有 NewAPI/Sub2API 渠道价格和倍率的可信确认、不可覆盖来源版本、币种换算及账单差异核对。
- 没有账号共享余额、在途费用预留、保守余额下限和按 SLA 等级保留容量。
- 没有多层故障域及故障域相关异常联动。
- 没有连续会话前缀 TTFT 账目、缓存损失预测和实际成功成本。
- 没有真实业务测活资格、探索预算、冷却后再次试错和独立探索账目。
- 首字前回退是串行切换，不是并行稳定资源接管；没有重复费用完整核算。
- 没有完整的租户 SLA 目标、错误预算、策略版本、事件生命周期和恢复机制。

这些缺口仍要求独立 SLA 控制组件，并非安装插件即可直接获得。

## 3. 维护边界

OmniRoute 可以不运行 Electron/PWA，但其 `open-sse` 请求核心直接反向依赖主应用的数据库、用量、成本、Guardrail、Memory、额度和压缩模块。关键耦合可见：

- [chatCore imports](https://github.com/diegosouzapw/OmniRoute/blob/3ba3cd145eefafcb22efa16d6110daeb65c8df06/open-sse/handlers/chatCore.ts#L120)
- [Backend build](https://github.com/diegosouzapw/OmniRoute/blob/3ba3cd145eefafcb22efa16d6110daeb65c8df06/package.json)

因此可以整体部署，但不适合抽取出一个小型、职责稳定的网关核心。压缩、Memory、MCP/A2A、桌面管理和多种产品逻辑扩大了安全审查、回归测试和上游升级范围。

## 4. 不推荐原因

OmniRoute 的 MIT 许可证和插件系统优于多数候选，但它没有减少价格、余额、故障域、会话预算、业务测活和 SLA 账本这些主要定制工作；动态首字预算和重复费用仍会进入关键请求路径。与 AxonHub 相比，它同时引入更大的非目标功能面和更强的内部耦合。

**结论：不作为最终基础。** 保留为验证插件式外部控制的参考实现，不进入正式采用方案。
