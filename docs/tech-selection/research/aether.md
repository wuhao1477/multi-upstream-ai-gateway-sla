# Aether 源码评估

| 项目 | 内容 |
| --- | --- |
| 仓库 | `fawney19/Aether` |
| 评估提交 | `7756c0913f2e0089575d019f1ca90d9867f35c52`（`main`） |
| 最近发布 | `v0.7.11` |
| 许可因素 | 按本轮要求，不参与排序 |
| 结论 | 原生领域能力强，列为优先候选；不作为最终推荐 |

## 1. 技术能力

Aether 是候选中领域模型覆盖最广的项目：

- Provider、Endpoint、Key、Model、Pool、Routing Group 和主体 Binding 均有管理 API。
- Key 可登记能力、优先级、倍率、模型权限、RPM、并发、有效期、费用、健康和熔断状态。
- Provider 额度快照包含 `billing_type`、`monthly_quota_usd`、`monthly_used_usd`、`quota_reset_day` 和 `quota_expires_at_unix_secs`，能表达月度额度、重置日和到期时间。
- 有动态候选规划、候选耗尽记录、首字 Watchdog、串行候选切换和客户端断开后的上游取消。
- 有会话亲和、缓存分析、用量、额度、费用、路由版本和管理审计。
- 对 NewAPI/Sub2API 的管理结构和余额解析比其他候选完整。
- 外部调用方可在授权后通过 `x-aether-scheduler-group` 逐请求选择预建 Routing Group，并用 `allowed_keys` 限制真实 Key。

关键源码：

- [AI 路由入口](https://github.com/fawney19/Aether/blob/7756c0913f2e0089575d019f1ca90d9867f35c52/apps/aether-gateway/src/api/ai/registry.rs#L10)
- [请求准入和认证](https://github.com/fawney19/Aether/blob/7756c0913f2e0089575d019f1ca90d9867f35c52/apps/aether-gateway/src/handlers/proxy/mod.rs#L894)
- [动态候选循环](https://github.com/fawney19/Aether/blob/7756c0913f2e0089575d019f1ca90d9867f35c52/apps/aether-gateway/src/executor/candidate_loop.rs#L106)
- [流式候选 Watchdog](https://github.com/fawney19/Aether/blob/7756c0913f2e0089575d019f1ca90d9867f35c52/apps/aether-gateway/src/executor/candidate_loop.rs#L765)
- [断开后取消上游](https://github.com/fawney19/Aether/blob/7756c0913f2e0089575d019f1ca90d9867f35c52/apps/aether-gateway/src/execution_runtime/stream/execution.rs#L1187)
- [Key 能力、额度和健康字段](https://github.com/fawney19/Aether/blob/7756c0913f2e0089575d019f1ca90d9867f35c52/crates/aether-data/contracts/src/repository/provider_catalog/types.rs#L248)
- [会话亲和](https://github.com/fawney19/Aether/blob/7756c0913f2e0089575d019f1ca90d9867f35c52/apps/aether-gateway/src/client_session_affinity.rs#L6)
- [路由组版本和发布](https://github.com/fawney19/Aether/blob/7756c0913f2e0089575d019f1ca90d9867f35c52/apps/aether-gateway/src/handlers/admin/routing/mod.rs#L82)
- [逐请求路由组选择](https://github.com/fawney19/Aether/blob/7756c0913f2e0089575d019f1ca90d9867f35c52/apps/aether-gateway/src/routing/selection.rs#L7)
- [Provider/Key 限制](https://github.com/fawney19/Aether/blob/7756c0913f2e0089575d019f1ca90d9867f35c52/crates/aether-routing-core/src/model.rs#L36)
- [Provider 额度模型](https://github.com/fawney19/Aether/blob/7756c0913f2e0089575d019f1ca90d9867f35c52/crates/aether-wallet/src/quota.rs#L24)
- [Provider 额度候选过滤](https://github.com/fawney19/Aether/blob/7756c0913f2e0089575d019f1ca90d9867f35c52/crates/aether-scheduler-core/src/provider.rs#L7)

## 2. 与 PRD 的差距

Aether 不能直接满足完整 PRD：

- 未确认定时采集上游模型价格、不可覆盖价格版本、涨降价可信确认、币种换算和扣费差异停路由。
- 共享余额/订阅关系和费用预留需要补充；没有固定费用分摊、预计到期未用额度、订阅引流上限和按 SLA 等级保留接管容量。
- 没有任意多层故障域和跨域接管策略。
- 没有连续会话前缀 TTFT 预算，以及包含失败、取消、缓存损失和重复费用的实际成功成本。
- 后台 Self-check/Quota Probe 不是带五级预算和资格判断的真实业务测活。
- 内部首字接管为串行，且并非所有流式路径都能在提交前切换；没有并行接管、取消确认和重复费用完整账本。
- 没有通用 SLA 事件 Webhook；源码中的回调主要用于支付、OAuth 入站和 Bark 推送。

Aether 的管理 API 能降低外部控制成本，但价格可信度、真实业务探索、会话账目和故障域仍需新增产品能力。

### 订阅额度可靠性

Aether 的字段覆盖明显强于一般网关，但当前 `should_skip_provider_quota` 只排除月度额度已耗尽的 Provider。其时间参数 `_now_unix_secs` 未参与判断，已到 `quota_expires_at_unix_secs` 的额度仍不会被过滤；相应测试也明确允许过期记录继续参与候选。因此这些字段可以作为外部订阅账本的采集来源，不能直接作为 FR-033～039 的准入依据。

## 3. 扩展边界

Provider、Key、路由、额度、用量和审计大多可通过 L0 管理 API 操作。推荐预建“一个真实 Key 一个 Routing Group”，为内部 SLA API Key 建立允许显式选择的 Binding，再由适配器注入调度组请求头。

该路径有两个明确限制：

- Provider 启用 Aether Pool 时，候选解析可能跳过路由组的 Key 过滤；需要 Key 粒度归因的资源不得使用内部 Pool，见 [候选过滤](https://github.com/fawney19/Aether/blob/7756c0913f2e0089575d019f1ca90d9867f35c52/apps/aether-gateway/src/ai_serving/planner/candidate_resolution.rs#L401)。
- 普通同格式 SSE 路径不能普遍保证在首个用户可见内容前完成内部切换。动态会话 TTFT 必须由外部同步网关预读 Aether 流、取消并重新选择 Routing Group。

项目有编译期 Adapter 和 Repository Trait，但没有运行时插件装载机制；若把完整 RoutePlan、动态逐候选期限或精确 Pool 成员选择写入 Aether，仍会进入 L2/L3。

## 4. 采用判断

**列入优先候选，但不作为最终推荐。** Aether 的资源、价格、余额、订阅字段、候选和用量模型非常接近本项目需求，L0 Routing Group 适配也成立。但过期额度过滤不能作为可靠准入，通用共享订阅池、固定费用分摊和到期未用预测仍需外部实现；普通流式路径也缺少稳定的首个有效内容提交前控制边界。因此它仍不能替代 AxonHub + 外部 SLA 决策网关。
