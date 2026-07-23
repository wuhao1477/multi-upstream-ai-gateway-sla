# 选型复审：AxonHub / ccLoad 之外是否存在更优执行数据面（2026-07-23）

| 项目 | 内容 |
| --- | --- |
| 复审日期 | 2026-07-23 |
| 触发 | 项目负责人要求"继续调研其他开源项目，评估有没有比 AxonHub/ccLoad 更好的选择" |
| 方法 | 网络检索 2026 年网关景观 + 对候选 README/文档做硬判据定向核查（未做源码级评估） |
| 结论 | **未发现更优选择，维持 AxonHub 唯一推荐 + ccLoad 轻量备选**；两个观察项见 §5 |
| 证据强度 | 景观级/文档级筛查。若某候选未来要升格为正式候选，须按 [00-repository-baseline](./00-repository-baseline.md) 流程做固定提交源码级评估 |

## 1. 复审用的硬判据（源自既有选型与 ISSUE-001 运行时结论）

新候选必须在这些点上**优于或至少等于** AxonHub 才值得替换：

1. **L0 自含**：不改源码，管理 REST/GraphQL API 可编程建渠道/Key，控制面自含（不依赖托管服务）。
2. **单资源绑定可外控**：一个 Key/请求可钉死到恰好一个渠道+Key+URL，无隐藏轮换/回退（ISSUE-001 假设 1 已在 AxonHub 运行时证实）。
3. **逐 Attempt 账本**：每次尝试独立记录（上游请求 ID、逐尝试 metrics、用量/费用/缓存 Token），API 可查、可关联（假设 2/4 已证实）。
4. **取消可传播/可对账**（假设 2 已证实；ccLoad 假设 6 补验证实取消传播止损）。
5. **订阅类上游兼容**：NewAPI/Sub2API 二开中转站 + Claude Code/Codex OAuth 订阅渠道（AxonHub 原生 channel_type + provider quota）。
6. 轻量部署（SQLite 可用）、100～1000 QPS、活跃维护。

> 首字语义不作为判据——两候选均不可信（假设 3/6），自研核心自算（FR/AC 已定），任何新候选同样按"不采信"处理。

## 2. 本轮新筛查候选与判定

上轮已评估 9 家（见 [TECHNICAL-SELECTION](../TECHNICAL-SELECTION.md)）。本轮新筛 10+ 家：

| 候选 | 定位 | 硬判据核查结果 | 判定 |
| --- | --- | --- | --- |
| **LiteLLM**（54.4k★，Python+Postgres） | 西方生态事实标准代理 | 虚拟 Key/预算/回退齐全；但**逐 Attempt 账本无证据**（观测走 Langfuse 等外部回调）、无订阅 OAuth 上游、单资源绑定需"单 deployment 模型组"迂回构造；Python+Postgres 偏重 | ❌ 不更优 |
| **Bifrost**（6.7k★，Go，Apache-2.0） | 高性能网关 | 性能强、API 驱动配置；但逐 Attempt 账本、按 Key 禁回退、订阅上游**均无文档证据**；观测面向 Prometheus/tracing 而非账务 | ❌ 不更优（观察项） |
| **Portkey Gateway**（12.5k★，TS，MIT） | 2026-03 全量开源 | **OSS 只含数据面**：分析、日志看板、Key 管理仍在托管/企业版；不满足"控制面自含 L0" | ❌ 排除（观察 2.0 合并进展） |
| **Helicone AI Gateway**（Rust） | 观测优先网关 | 被 Mintlify 收购后**进入维护模式** | ❌ 排除 |
| **TensorZero**（Rust） | 网关+观测+优化 | **项目已停更** | ❌ 排除 |
| **gpt-load**（6.3k★，Go，MIT） | Key 池轮换透明代理 | 组内可单 Key 构造绑定、重试可配；但**无 token/成本账本、无管理 API 文档、无逐 Attempt 记录**——比 ccLoad 还弱（ccLoad 有细分缓存/成本、延迟提交、取消传播） | ❌ 不更优 |
| **uni-api**（1.2k★，Python） | 单文件配置式路由 | `fixed_priority` 可钉渠道、`AUTO_RETRY:false` 可禁重试；但**纯配置文件管理**（改渠道需改文件重启），无动态管理 API，审计弱 | ❌ 不更优 |
| **claude-relay-service**（12.4k★，Node+Redis） | Claude/Codex 订阅账号中继 | 订阅利基与 AxonHub 重叠；但**账号池"智能切换"、无单账号钉死**、无逐 Attempt 账本、无管理 API 文档、仅 Redis 无关系账本 | ❌ 不更优（利基被 sub2api 评估覆盖） |
| **Manifest**（7.3k★，TS+Postgres） | 复杂度自动路由网关（含 coding-tier 订阅上游） | **网关自己做路由决策**（complexity/`auto` 端点）——与本架构"决策在自研核心、网关做听话执行面"相反；绑定/账本无证据 | ❌ 架构相反 |
| **Chat Nio / Veloera / VoAPI / done-hub** | NewAPI 系二开/计费面板 | 与 NewAPI 同族同判：普通调用方不能指定 Channel、Channel 内多 Key 轮换、无统一尝试账目 | ❌ 同 NewAPI 结论 |
| **Kong / APISIX / Higress / Envoy AI GW**（基础设施类） | 平台级 AI 网关 | 面向企业流量治理；渠道/Key 账本语义、订阅站适配非其领域；配置经 CRD/声明式而非管理 API；对个人 100～1000 QPS 场景过重 | ❌ 类别不匹配 |
| **Plano / archgw / semantic-router** | Agent 代理 / 提示路由 / 语义路由 | 解决的是"选哪个模型"，非"执行面账本与绑定"，问题域不同 | ❌ 问题域不同 |

## 3. 关键发现

1. **本项目的判据组合在西方生态里是异类**。LiteLLM/Bifrost/Portkey 解决"统一接入 + 回退 + 预算"，普遍**把回退/轮换当卖点**，而我们恰恰要禁用它并要求逐 Attempt 留痕。没有一家以"可外控的单资源绑定 + 财务级尝试账本"为一等公民。
2. **订阅中继利基里 AxonHub 仍是账本最完整的**。同利基（sub2api、claude-relay-service）要么不能精确钉账号、要么无关系型逐尝试账本；AxonHub 的 `request_execution` + `usage_log` + provider quota 组合在被筛项目中无对手。
3. **AxonHub 维护状态健康且方向有利**：beta5（2026-07-11）为最新，约 1～2 周一版；beta3～5 新增渠道级可重试状态码、响应超时重试、每渠道并发上限、熔断、缓存命中监控——都是执行面能力增强，与我们的 L0 契约方向一致。风险不变：仍无稳定 1.0，升级须跑 [verify/](../../../verify/README.md) 准入检查。
4. **两家退出市场**（TensorZero 停更、Helicone 维护模式）反而佐证：独立网关赛道整合中，选活跃度高的 AxonHub/ccLoad 组合方向正确。

## 4. 结论

**维持原选型**：AxonHub（唯一推荐执行数据面，L0）+ ccLoad（轻量备选/退路，ISSUE-001 假设 6 已运行时验证稳固）。本轮 10+ 新候选无一在硬判据上更优；多数在"逐 Attempt 账本"和"订阅上游"两点直接失分。

## 5. 观察项（不构成行动）

| 观察项 | 触发条件 | 动作 |
| --- | --- | --- |
| **Portkey Gateway 2.0** | 若企业控制面（日志/分析/Key 管理）真正全量并入 OSS | 重新按 §1 判据评估其账本与绑定能力 |
| **Bifrost** | 若 AxonHub 停滞或 1.0 长期难产,需要 Go 系高性能替代时 | 做固定提交源码级评估（绑定/账本两点定生死） |

## 来源

- [wavect.io: LLM Gateways Compared 2026](https://wavect.io/blog/llm-gateway-router-comparison-2026/)、[klymentiev.com: LLM Gateway 2026](https://klymentiev.com/blog/llm-gateway-guide)（TensorZero 停更、Helicone 维护模式、Portkey 2026-03 开源）
- GitHub：[LiteLLM](https://github.com/BerriAI/litellm)、[Bifrost](https://github.com/maximhq/bifrost)、[Portkey Gateway](https://github.com/Portkey-AI/gateway)、[gpt-load](https://github.com/tbphp/gpt-load)、[uni-api](https://github.com/yym68686/uni-api)、[claude-relay-service](https://github.com/Wei-Shaw/claude-relay-service)、[Manifest](https://github.com/mnfst/manifest)、[awesome-ai-gateway](https://github.com/cuihuan/awesome-ai-gateway)、[AxonHub releases](https://github.com/looplj/axonhub/releases)
- 国内景观：[SegmentFault 2026 中转平台对比](https://segmentfault.com/a/1190000048025127)、[腾讯云 2026 API 中转指南](https://cloud.tencent.com/developer/article/2619750)
