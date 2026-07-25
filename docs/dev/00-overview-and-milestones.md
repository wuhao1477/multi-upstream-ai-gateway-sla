# 开发总览与里程碑（v0.1 草案）

| 项目 | 内容 |
| --- | --- |
| 状态 | 草案，待评审 |
| 日期 | 2026-07-23 |
| 输入 | [PRD v1.3](../PRD.md)、[选型结论](../tech-selection/TECHNICAL-SELECTION.md)、[ISSUE-001 运行时结论](../issues/ISSUE-001-tech-assumption-verification.md)、[ISSUE-002 采集适配器设计](../issues/ISSUE-002-collector-adapter-design.md) |
| 技术栈决策（2026-07-23 确认） | 自研核心 **Go**（存储层 **pgx + sqlc**）；状态存储 **PostgreSQL 单库**（一期不引 Redis）；部署 **单机 Docker Compose**（LB + **≥2 核心实例** + PG + collector，**无外部网关**）；文档按 docs/dev/ 分篇 |
| 主力客户端（2026-07-23 确认） | **Codex CLI**（走 **OpenAI Responses** 协议，自带 `session_id`/`conversation_id` 头与 `prompt_cache_key`）。据此：一期入站协议维持 CC + Responses 不扩（[02 §4.5](./02-data-model.md)）；上游对接走**字节级透传**保真 reasoning 与事件生命周期（[03](./03-upstream-layer.md)、[13 §1](./13-research-reassessment.md)）；自研层不做隐藏重试，一次外部调用严格对应一次上游调用（FR-119） |

## 1. 要建什么（一句话）

**客户端（主力 Codex CLI）→ 自研 SLA 决策核心（Go，多实例）→ 自研上游透传层 → ~20 个上游**；旁路异步跑三家族采集器（NewAPI/Sub2API/ASXS）维护价格/余额/订阅台账。调度决策、账本、TTFT 判定全在自研核心；上游对接为**字节级透传**（[11 转向](./11-decision-full-selfbuilt.md)、[03](./03-upstream-layer.md)）。

## 2. 硬约束清单（开发期不可违反）

| # | 约束 | 来源 |
| --- | --- | --- |
| 1 | 决策延迟 P99≤50ms、核心可用性≥99.95%、多实例、禁旁路直连上游 | FR-110 |
| 2 | 只支持 OpenAI Chat Completions + Responses，流式/工具调用/多模态透传 | FR-111、AC-26 |
| 3 | 一期不存请求正文/上下文/头/体；账本元数据≥180 天 | FR-112 |
| 4 | TTFT 自算内容感知（排除 role-only/空 SSE/心跳），不采信网关首字字段 | AC-31、ISSUE-001 假设 3/6 |
| 5 | 取消/失败按 `errorMessage` 归并对账（网关 `canceled` 仅指客户端取消） | AC-30、假设 2 |
| 6 | 配额未知渠道可配置过滤，默认保守排除 | FR-118、假设 5 |
| 7 | 记账不得假设"一次调用=一次上游用量"，须识别并补算同资源隐藏重试 | FR-119、假设 6 |
| 8 | 并发重试默认全局禁止；测活预算全局≤2% 月费用；策略全部可配置+关键项二次确认 | 参数 3/6、FR-115 |
| 9 | 模型别名即策略载体（别名映射 SLA 等级与测活资格） | FR-062/117、AC-25 |
| 10 | 订阅双倍率：用满倍率调度、实际倍率账务 | 参数 14、AC-24 |
| 11 | **Responses 走字节级透传，不做解析-重组**（三个独立项目走重组路线均在 Codex 兼容性翻车） | [13 §5](./13-research-reassessment.md) |

## 3. 里程碑

| 里程碑 | 交付 | 覆盖重点 | 退出标准 |
| --- | --- | --- | --- |
| **M0 骨架** | Go 单仓工程骨架、compose（Caddy LB + 2×core + PG + collector）、配置与别名→策略模型、CI | FR-115/117 | compose 一键起；健康检查通过；别名策略可加载 |
| **M1 上游直连 + 账本 v1** | 自研上游透传层（字节透传 + 旁路观察）；OpenAI CC/Responses；Attempt 账本落 PG（单一真相源） | FR-111/119、AC-26/31/32 | 真实上游 Responses **35 字段与 reasoning item 零丢失**；账本 usage 来自旁路终帧 |
| **M2 流式 SLA 核心** | 内容感知 TTFT、动态期限、首字前接管、mid-stream 取消传播、errorMessage 归并、隐藏重试补算 | 硬约束 4/5/7，AC-30～32 | verify/ 式 mock 场景全绿（role-only/心跳/空 SSE/慢首字/中断） |
| **M3 元数据与订阅** | 三家族采集器、价格版本、余额信号识别、订阅台账+双倍率+倾斜三道闸 | FR-010/011/027/033～039、AC-24/28/29 | 4 家实测站点采集跑通；双倍率调度用例通过 |
| **M4 经营与验收** | 测活预算/冷却/样本门槛、容量保留、告警 P1～P3、AC-01～32 验收矩阵、100～1000 QPS 压测 | 参数 3/11/12/16、FR-114 | 验收矩阵全绿；压测达标（P99≤50ms 决策开销） |

里程碑串行推进、每个可独立评审；M2 是风险最高的一段（流式接管），其 mock 场景直接复用 verify/ 的构造经验。

## 4. 文档索引

| 文档 | 内容 | 状态 |
| --- | --- | --- |
| 00（本篇） | 总览、硬约束、里程碑 | 草案 |
| [01 架构设计](./01-architecture.md) | 组件拓扑、sla-core 模块、上游直连要点、请求时序 | 草案 |
| [02 数据模型](./02-data-model.md) | PG 单库 schema：资源注册、别名策略、价格版本、逐 Attempt 账本、订阅台账（双倍率）、健康/冷却、采集/余额、告警、保留分区 | 草案 |
| [03 上游对接层](./03-upstream-layer.md) | 自研直连：字节透传 + 旁路观察、Codex 兼容硬约束、头部透传、取消与超时、协议能力探测 | 草案 |
| [04 采集器契约](./04-collector-adapter.md) | CollectorAdapter 接口 + 三家族（NewAPI/Sub2API/ASXS）字段映射 + 凭证生命周期状态机 | 草案 |
| [05 调度与经营策略](./05-scheduling-and-operations.md) | selector 候选过滤/排序/RoutePlan + steward 测活预算/冷却/订阅倾斜/容量保留/告警/错误预算 | 草案 |
| [06 部署与运维](./06-deployment-and-operations.md) | 单机 Compose 拓扑、上游直连与选主、版本治理、备份保留、可观测、M0 部署清单 | 草案 |
| [07 AxonHub 运行时实测](./07-axonhub-runtime-probes.md) | ⚠️ **历史记录**：其 Responses round-trip 损耗实测是 [11 转向](./11-decision-full-selfbuilt.md)的直接依据；§3bis 的真实上游基线转为自研透传层的保真目标 | 历史 |
| [08 参考项目评估：zhfeng1/ai-gateway](./08-ref-eval-zhfeng1-ai-gateway.md) | 源码级评估：TTFT 品类通病第三方佐证、网关开销测法、TPS 分母口径；明确不吸收清单 | ✅ 已评估 |
| [09 管理 API 与设置面](./09-admin-api.md) | `/admin/*` 配置读写 API、关键项二次确认流程、策略元数据（FR-115 操作面） | 草案 |
| [10 工程结构与构建](./10-project-structure.md) | Go 单仓布局、包边界（对齐 01 模块）、构建/CI 门禁（含 FR-112 与假设2 守卫）、M0 交付物映射 | 草案 |
| [11 架构转向决策](./11-decision-full-selfbuilt.md) | **移除外部网关、彻底自研**的决策、依据、影响面与返工范围 | ✅ 已定 |
| [12 可调试性设计](./12-debuggability.md) | 自研透传层的排障能力：L0/L1/L2 分层，FR-112 约束下只存元数据 | 草案 |
| [13 调研资产重审](./13-research-reassessment.md) | 转向后重审全部调研；Codex 三条硬约束；三项目在同一处翻车 → 字节透传 | ✅ 已重审 |
