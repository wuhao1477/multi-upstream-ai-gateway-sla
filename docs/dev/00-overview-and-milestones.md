# 开发总览与里程碑（v0.1 草案）

| 项目 | 内容 |
| --- | --- |
| 状态 | ✅ **v1.0 基线（2026-07-26 冻结）** —— 经 28 轮对抗性审查 + 2 轮开发视角走查 + PM 开工前裁决；变更须走版本记录 |
| 日期 | 2026-07-23 |
| 输入 | [PRD v1.3](../PRD.md)（**§2.1 为分期真相源**）、[11 转向决策](./11-decision-full-selfbuilt.md)、[13 调研重审](./13-research-reassessment.md)、[15 范围与隐患](./15-scope-and-preflight.md)、[ISSUE-002 采集适配器设计](../issues/ISSUE-002-collector-adapter-design.md) |
| 技术栈决策（2026-07-23 确认） | 自研核心 **Go**（存储层 **pgx + sqlc**）；状态存储 **PostgreSQL 单库**（一期不引 Redis）；部署 **单机 Docker Compose**（LB + **≥2 核心实例** + PG + collector，**无外部网关**）；文档按 docs/dev/ 分篇 |
| 交付分期 | **以 [PRD §2.1](../PRD.md) 为唯一真相源**：一期 = SLA 自研网关；二期 = 订阅制/主动测活/缓存进阶/多 SLA 等级/外部告警。本篇里程碑与 [14 验收矩阵](./14-acceptance-matrix.md) 均由其派生 |
| 主力客户端（2026-07-23 确认） | **Codex CLI**（走 **OpenAI Responses** 协议，自带 `session_id`/`conversation_id` 头与 `prompt_cache_key`）。据此：一期入站协议维持 CC + Responses 不扩（[02 §4.5](./02-data-model.md)）；上游对接走**字节级透传**保真 reasoning 与事件生命周期（[03](./03-upstream-layer.md)、[13 §1](./13-research-reassessment.md)）；自研层不做隐藏重试，一次外部调用严格对应一次上游调用（FR-119） |

## 1. 要建什么（一句话）

**客户端（主力 Codex CLI）→ 自研 SLA 决策核心（Go，多实例）→ 自研上游透传层 → ~20 个上游**；旁路异步跑三家族采集器（NewAPI/Sub2API/ASXS）维护价格/余额/Key 额度（⏭ 订阅台账二期）。调度决策、账本、TTFT 判定全在自研核心；上游对接为**字节级透传**（[11 转向](./11-decision-full-selfbuilt.md)、[03](./03-upstream-layer.md)）。

## 2. 硬约束清单（开发期不可违反）

| # | 约束 | 来源 |
| --- | --- | --- |
| 1 | 决策延迟 P99≤50ms（口径=总延迟−上游耗时）、**进程级冗余**（任一 core 宕机不中断）、禁旁路直连上游。⚠️ 一期**不承诺 99.95% 整体可用性**（单机 Caddy/PG/主机为共享故障点） | FR-110、AC-27/34 |
| 2 | 只支持 OpenAI Chat Completions + Responses，流式/工具调用/多模态透传 | FR-111、AC-26 |
| 3 | 一期不存请求正文/上下文/头/体；账本元数据≥180 天 | FR-112 |
| 4 | TTFT 由**旁路观察自算**（内容感知，排除 role-only/空 SSE/心跳注释），**不采信任何上游或中间层回传的首字字段** | AC-31、[03 §3.2](./03-upstream-layer.md) |
| 5 | 取消/失败口径统一归并：客户端断开 / 上游断流 / SLA 取消 / 上游错误 分别落 `cancel_reason`，原文存 `error_message` | AC-30、[02 §4.2](./02-data-model.md) |
| 6 | 配额未知渠道可配置过滤，默认保守排除 | FR-118、假设 5 |
| 7 | **一次外部调用 = 一次上游调用**：自研上游层不做隐藏重试，每次重试/接管作为独立 Attempt 显式落账 | FR-119、[03 §1](./03-upstream-layer.md) |
| 8 | 并发重试默认全局禁止；策略全部可配置 + 关键项二次确认；**测活预算体系属一期**（2026-07-26 调整） | 参数 6、FR-115、FR-063 |
| 9 | 模型别名即策略载体（别名映射策略；一期为单级 SLA + 测活资格标记） | FR-062/117、AC-25 |
| 10 | ⏭ **二期**：订阅双倍率（用满倍率调度、实际倍率账务）—— 一期成本排序仅用普通渠道价格版本×倍率。现有订阅渠道基本已到期且暂不续费，一期无触发场景 | 参数 14、AC-24（[PRD §2.1](../PRD.md) 二期） |
| 11 | **Responses 走字节级透传，不做解析-重组**（三个独立项目走重组路线均在 Codex 兼容性翻车） | [13 §5](./13-research-reassessment.md) |
| 13 | **入站鉴权与上游凭证严格隔离**：`/v1/*` 用独立的 `gateway_clients` 凭证（只存哈希），**严禁复用上游 Key** | FR-120、AC-33 |
| 12 | **程序自身抗压优先**：上游可无限扩充，自研核心扛不住则一切无意义。最低门槛 **1000 并发用户高强度持续使用**；压测用 mock 上游、只计决策开销，并须探测容量拐点 | [14 §2bis](./14-acceptance-matrix.md)、FR-110/114 |

## 3. 里程碑

| 里程碑 | 交付 | 覆盖重点 | 退出标准 |
| --- | --- | --- | --- |
| **M0 骨架** | Go 单仓工程骨架、compose（Caddy LB + 2×core + PG + collector）、配置与别名→策略模型、CI | FR-115/117 | ① `docker compose up` 后 `/healthz` 全绿；② 停任一 core 实例，30 个连续请求**全部成功**（AC-27）；③ 加载**固定种子**（见下）并经 `GET /admin/config` 回显一致；④ CI 全绿（含 FR-112 不可存列断言 + **DDL 真跑**）；⑤ **AC-27 全通过 + AC-33 的 M0 子集**（见 [14](./14-acceptance-matrix.md) AC-33 分期说明）

> ⚠️ **M0 边界（开发视角审查第 27 轮 [P0] 澄清）**：M0 **只做骨架与入站侧**——工程结构、compose、`/healthz`、`/admin/config` 与 `/admin/clients`、迁移与 CI、最小入站鉴权。
> **不属于 M0**：上游透传、Responses 35 字段 diff、Codex 实机、mock 场景集、日费用预留/结算/人工修正——这些依赖上游对接层与账本，**全部属 M1**（[03 §10](./03-upstream-layer.md)）。
> [06 §7 部署清单](./06-deployment-and-operations.md) 中列出的上游相关项同属 M1，勿按 M0 验收。
>
> **M0 固定种子（冻结，开发照抄即可）** —— 迁移脚本 `migrations/0002_seed_m0.sql`：
>
> **① 模型**（1 条）：`canonical_name='gpt-5.5'`，`max_input_tokens=272000`、`max_output_tokens=128000`
> （两列**必填**，为空则该模型全部 binding 不进候选，[02 §2bis](./02-data-model.md)）。
>
> **② 策略 + 别名**（3 组，一一对应）：
>
> | 别名 | 策略 `name` | `is_committed` | `canary_eligible` | `probe_allowed` | `conflict_order` | `no_resource_wait_ms` |
> | --- | --- | --- | --- | --- | --- | --- |
> | `gpt-5.5-sla-1` | `committed-default` | **true** | false | false | `["sla","cache","cost"]` | 15000 |
> | `gpt-5.5` | `standard` | false | **true** | **true** | `["sla","cost","cache"]` | 5000 |
> | `gpt-5.5-cheap` | `cost-first` | false | **true** | **true** | `["cost","cache","sla"]` | 0 |
>
> 三行都满足 `CHECK (NOT (is_committed AND (canary_eligible OR probe_allowed)))`。
> **金/银/铜只是这三行的历史别名，不再承载语义** —— 调度一律读上表的布尔字段。
>
> **③ `sla_targets`（唯一一行，对应唯一承诺策略）**：
>
> | `policy_id` | `metric` | `target_value` | `window_spec` | 其余维度 |
> | --- | --- | --- | --- | --- |
> | → `committed-default` | `ttft_p95_ms` | **5000** | `1d` | `model_id`/`request_type`/`tenant_id` 全 NULL（= 不细分） |
>
> - **只配一档**：一期"单级 SLA"= 单一**承诺**等级；另两条策略 `is_committed=false`，**不得**有 `sla_targets` 行（空承诺）。
> - **5000ms 的来源**：前缀平均 ≤10s 的目标下，单跳 P95 取 5s 为接管留余量（[05 §1.3](./05-scheduling-and-operations.md)）。
> - **AC-06/AC-09 的"TTFT 达标"= 该行**：`ttft_p95_ms <= 5000`，窗口 `1d`。**判据唯一，不得另立。**
>
> **④ 种子校验断言**（CI，M0 门禁）：
> `is_committed=true` 的策略**必须**有对应 `sla_targets` 行；`is_committed=false` 的**必须没有**。
| **M1 上游直连 + 账本 v1** | 自研上游透传层（字节透传 + 旁路观察）；OpenAI CC/Responses；Attempt 账本落 PG（单一真相源） | FR-111/119、AC-26/31/32 | ① REAL：Responses 响应与直连基线逐字段 diff，**35 字段与 reasoning item 零丢失**；② 账本 usage 来自旁路终帧且与上游一致；③ **AC-01/16/26/30/31/32 + AC-35 全通过**（[14 验收矩阵](./14-acceptance-matrix.md) M1 集，共 7 条）；④ **AC-35 崩溃恢复不可跳过**——账本首版必须同时交付 request 级恢复扫描与两阶段关单，否则 K1~K4 全部不可验 |
| **M2 流式 SLA 核心** | 内容感知 TTFT、动态期限、首字前接管、mid-stream 取消传播、取消口径归并 | 硬约束 4/5/7，AC-30～32 | ① MOCK 场景全绿（role-only/心跳/空 SSE/慢首字/中断/abort）；② **AC-06/07/12/15/25 全通过**（[14](./14-acceptance-matrix.md) M2 集，共 5 条；**AC-07 缓存切换损失预测已于 2026-07-26 拉入一期**）；③ 内容感知 TTFT 在三类元事件场景下**均不出现 ≈0ms** |
| **M3 元数据采集** | 三家族采集器、价格版本、余额信号识别（**订阅台账/双倍率/倾斜三道闸移入二期**，[15](./15-scope-and-preflight.md)） | FR-010/011/020～027、AC-28/29 | ① 4 家实测站点采集跑通且 `Capabilities()` 与 [04 §3.4](./04-collector-adapter.md) 矩阵一致；② **AC-02/03/04/05/17/19/28/29 全通过**（订阅相关 AC-20~24 移入二期） |
| **M4 经营与验收** | 冷却/样本门槛、**渠道验证双轨**（canary FR-121 + **主动测活 FR-060~067/070~075**，均为跨实例原子 claim；[05 §2.1bis](./05-scheduling-and-operations.md) 冻结归属为 M4，M2 不含测活）、容量保留、告警 P1～P3（落库）、一期验收矩阵收口、100～1000 QPS 压测 | 参数 5/11/12/16、FR-114、**FR-121** | ① **AC-08/09/10/11/13/14/18/34/36 全通过**（[14](./14-acceptance-matrix.md) M4 集，共 9 条；**AC-09/10 主动测活已于 2026-07-26 拉入一期**），且累计**一期 31 条 AC 全绿**且证据留档（二期 5 条：订阅 AC-20~24 不在本期；31+5=36 与 [PRD](../PRD.md) 对齐）；② **AC-08 canary 闭环通过**（新建/冷却期满渠道在无全局故障下完成晋级，硬上限双实例不被突破）；③ LOAD：**1000 并发用户高强度稳态 30 分钟**，决策开销 P99 ≤50ms、错误率 <0.01%、无内存/goroutine/FD 泄漏；④ 完成容量上限探测并记录拐点（[14 §2bis](./14-acceptance-matrix.md)） |

里程碑串行推进、每个可独立评审；M2 是风险最高的一段（流式接管），其 mock 场景直接复用 verify/ 的构造经验。

## 4. 文档索引

| 文档 | 内容 | 状态 |
| --- | --- | --- |
| 00（本篇） | 总览、硬约束、里程碑 | 草案 |
| [01 架构设计](./01-architecture.md) | 组件拓扑、sla-core 模块、上游直连要点、请求时序 | 草案 |
| [02 数据模型](./02-data-model.md) | PG 单库 schema：资源注册、别名策略、价格版本、逐 Attempt 账本、健康/冷却、采集/余额、告警、保留分区（订阅台账域建表保留、一期不写入） | 草案 |
| [03 上游对接层](./03-upstream-layer.md) | 自研直连：字节透传 + 旁路观察、Codex 兼容硬约束、头部透传、取消与超时、协议能力探测 | 草案 |
| [04 采集器契约](./04-collector-adapter.md) | CollectorAdapter 接口 + 三家族（NewAPI/Sub2API/ASXS）字段映射 + 凭证生命周期状态机 | 草案 |
| [05 调度与经营策略](./05-scheduling-and-operations.md) | selector 候选过滤/排序/RoutePlan + steward 冷却/容量保留/告警/错误预算（⏭ 测活预算与订阅倾斜二期） | 草案 |
| [06 部署与运维](./06-deployment-and-operations.md) | 单机 Compose 拓扑、上游直连与选主、版本治理、备份保留、可观测、M0 部署清单 | 草案 |
| [07 AxonHub 运行时实测](./07-axonhub-runtime-probes.md) | ⚠️ **历史记录**：其 Responses round-trip 损耗实测是 [11 转向](./11-decision-full-selfbuilt.md)的直接依据；§3bis 的真实上游基线转为自研透传层的保真目标 | 历史 |
| [08 参考项目评估：zhfeng1/ai-gateway](./08-ref-eval-zhfeng1-ai-gateway.md) | 源码级评估：TTFT 品类通病第三方佐证、网关开销测法、TPS 分母口径；明确不吸收清单 | ✅ 已评估 |
| [09 管理 API 与设置面](./09-admin-api.md) | `/admin/*` 配置读写 API、关键项二次确认流程、策略元数据（FR-115 操作面） | 草案 |
| [10 工程结构与构建](./10-project-structure.md) | Go 单仓布局、包边界（对齐 01 模块）、构建/CI 门禁（含 FR-112 与假设2 守卫）、M0 交付物映射 | 草案 |
| [11 架构转向决策](./11-decision-full-selfbuilt.md) | **移除外部网关、彻底自研**的决策、依据、影响面与返工范围 | ✅ 已定 |
| [12 可调试性设计](./12-debuggability.md) | 自研透传层的排障能力：L0/L1/L2 分层，FR-112 约束下只存元数据 | 草案 |
| [13 调研资产重审](./13-research-reassessment.md) | 转向后重审全部调研；Codex 三条硬约束；三项目在同一处翻车 → 字节透传 | ✅ 已重审 |
| [14 验收矩阵](./14-acceptance-matrix.md) | **AC → 里程碑 → 可执行判定方法**；PM 与开发的验收契约（**一期 31 条 + 二期 5 条 = 36**，与本文里程碑表及 [PRD](../PRD.md) 一致） | 草案（M1 前定稿） |
| [15 一期范围与开工前确认清单](./15-scope-and-preflight.md) | 一期/二期范围展开说明 + 16 项隐患确认结果（10 定论 + 6 验证点） | ✅ 已确认 |
