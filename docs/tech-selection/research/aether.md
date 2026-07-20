# Aether 源码评估

| 项目 | 内容 |
| --- | --- |
| 仓库 | `fawney19/Aether` |
| 评估提交 | `7756c0913f2e0089575d019f1ca90d9867f35c52`（`main`） |
| 最近发布 | `v0.7.11` |
| 许可证 | 自定义“非商业开源许可证” |
| 结论 | 技术能力较强，但被许可证硬性排除 |

## 1. 技术能力

Aether 是候选中领域模型覆盖最广的项目：

- Provider、Endpoint、Key、Model、Pool、Routing Group 和主体 Binding 均有管理 API。
- Key 可登记能力、优先级、倍率、模型权限、RPM、并发、有效期、费用、健康和熔断状态。
- 有动态候选规划、候选耗尽记录、首字 Watchdog、串行候选切换和客户端断开后的上游取消。
- 有会话亲和、缓存分析、用量、额度、费用、路由版本和管理审计。
- 对 NewAPI/Sub2API 的管理结构和余额解析比其他候选完整。

关键源码：

- [AI 路由入口](https://github.com/fawney19/Aether/blob/7756c0913f2e0089575d019f1ca90d9867f35c52/apps/aether-gateway/src/api/ai/registry.rs#L10)
- [请求准入和认证](https://github.com/fawney19/Aether/blob/7756c0913f2e0089575d019f1ca90d9867f35c52/apps/aether-gateway/src/handlers/proxy/mod.rs#L894)
- [动态候选循环](https://github.com/fawney19/Aether/blob/7756c0913f2e0089575d019f1ca90d9867f35c52/apps/aether-gateway/src/executor/candidate_loop.rs#L106)
- [流式候选 Watchdog](https://github.com/fawney19/Aether/blob/7756c0913f2e0089575d019f1ca90d9867f35c52/apps/aether-gateway/src/executor/candidate_loop.rs#L765)
- [断开后取消上游](https://github.com/fawney19/Aether/blob/7756c0913f2e0089575d019f1ca90d9867f35c52/apps/aether-gateway/src/execution_runtime/stream/execution.rs#L1187)
- [Key 能力、额度和健康字段](https://github.com/fawney19/Aether/blob/7756c0913f2e0089575d019f1ca90d9867f35c52/crates/aether-data/contracts/src/repository/provider_catalog/types.rs#L248)
- [会话亲和](https://github.com/fawney19/Aether/blob/7756c0913f2e0089575d019f1ca90d9867f35c52/apps/aether-gateway/src/client_session_affinity.rs#L6)
- [路由组版本和发布](https://github.com/fawney19/Aether/blob/7756c0913f2e0089575d019f1ca90d9867f35c52/apps/aether-gateway/src/handlers/admin/routing/mod.rs#L82)

## 2. 与 PRD 的差距

即使许可证问题解决，Aether 也不能直接满足完整 PRD：

- 未确认定时采集上游模型价格、不可覆盖价格版本、涨降价可信确认、币种换算和扣费差异停路由。
- 共享余额关系和费用预留需要补充；没有按 SLA 等级保留接管容量。
- 没有任意多层故障域和跨域接管策略。
- 没有连续会话前缀 TTFT 预算，以及包含失败、取消、缓存损失和重复费用的实际成功成本。
- 后台 Self-check/Quota Probe 不是带五级预算和资格判断的真实业务测活。
- 首字接管为串行切换，没有并行接管、取消确认和重复费用完整账本。
- 没有通用 SLA 事件 Webhook；源码中的回调主要用于支付、OAuth 入站和 Bark 推送。

Aether 的管理 API 能降低外部控制成本，但价格可信度、真实业务探索、会话账目和故障域仍需新增产品能力。

## 3. 扩展边界

Provider、Key、Pool、路由、额度、用量和审计大多可通过 L0 管理 API 操作。项目也有编译期 Adapter 和 Repository Trait，但没有运行时插件装载机制；需要改变首字等待、候选执行或账务语义时，仍会进入 L2/L3。

## 4. 许可证硬性阻碍

仓库不是 OSI 定义的开源许可证，而是自定义非商业许可：

- [LICENSE 第 5 行](https://github.com/fawney19/Aether/blob/7756c0913f2e0089575d019f1ca90d9867f35c52/LICENSE#L5) 授权使用、复制、修改和分发。
- [LICENSE 第 8 行](https://github.com/fawney19/Aether/blob/7756c0913f2e0089575d019f1ca90d9867f35c52/LICENSE#L8) 禁止盈利使用，包括付费服务和商业产品。
- [LICENSE 第 14 行](https://github.com/fawney19/Aether/blob/7756c0913f2e0089575d019f1ca90d9867f35c52/LICENSE#L14) 仅明确允许企业内部非盈利工具。
- [LICENSE 第 29 行](https://github.com/fawney19/Aether/blob/7756c0913f2e0089575d019f1ca90d9867f35c52/LICENSE#L29) 要求商业使用另行取得授权。

本项目面向有 SLA 责任的业务网关。在没有书面商业授权、且部署范围没有被法律确认属于许可所称“企业内部非盈利工具”前，不得把 Aether 作为默认基础。本报告不构成法律意见。

## 5. 不推荐原因

**结论：排除。** 原因是许可证不满足默认商业采用条件，而不是技术能力不足。即便后续单独获得授权，也必须重新评估授权范围、持续升级权利和定制代码归属，不能沿用本次结论自动转为采用。
