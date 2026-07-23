# 开发总览与里程碑（v0.1 草案）

| 项目 | 内容 |
| --- | --- |
| 状态 | 草案，待评审 |
| 日期 | 2026-07-23 |
| 输入 | [PRD v1.3](../PRD.md)、[选型结论](../tech-selection/TECHNICAL-SELECTION.md)、[ISSUE-001 运行时结论](../issues/ISSUE-001-tech-assumption-verification.md)、[ISSUE-002 采集适配器设计](../issues/ISSUE-002-collector-adapter-design.md) |
| 技术栈决策（2026-07-23 确认） | 自研核心 **Go**；状态存储 **PostgreSQL 单库**（一期不引 Redis）；部署 **单机 Docker Compose**（LB + 2 核心实例 + PG + AxonHub）；文档按 docs/dev/ 分篇 |

## 1. 要建什么（一句话）

**客户端 → 自研 SLA 决策核心（Go，多实例）→ GatewayAdapter → stock AxonHub（L0）→ ~20 个上游**；旁路异步跑三家族采集器（NewAPI/Sub2API/ASXS）维护价格/余额/订阅台账。所有调度决策、账本、TTFT 判定在自研核心；AxonHub 只做受控执行并回传逐尝试记录。

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
| 11 | AxonHub 锁定 `v1.0.0-beta5`；升级须先跑 [verify/](../../verify/README.md) 准入检查 | ISSUE-001 |

## 3. 里程碑

| 里程碑 | 交付 | 覆盖重点 | 退出标准 |
| --- | --- | --- | --- |
| **M0 骨架** | Go 单仓工程骨架、compose（Caddy LB + 2×core + PG + AxonHub beta5）、配置与别名→策略模型、CI | FR-115/117 | compose 一键起；健康检查通过；别名策略可加载 |
| **M1 受控执行 + 账本 v1** | OpenAI CC/Responses 透传；单渠道绑定执行（经 AxonHub Key-per-Channel）；Attempt 账本落 PG；与 AxonHub execution/usageLogs 对账 | FR-111、假设 1/4 契约、AC-26 | 请求钉死单渠道且账本能关联 AxonHub 逐尝试记录；AxonHub 自身重试策略已置零 |
| **M2 流式 SLA 核心** | 内容感知 TTFT、动态期限、首字前接管、mid-stream 取消传播、errorMessage 归并、隐藏重试补算 | 硬约束 4/5/7，AC-30～32 | verify/ 式 mock 场景全绿（role-only/心跳/空 SSE/慢首字/中断） |
| **M3 元数据与订阅** | 三家族采集器、价格版本、余额信号识别、订阅台账+双倍率+倾斜三道闸 | FR-010/011/027/033～039、AC-24/28/29 | 4 家实测站点采集跑通；双倍率调度用例通过 |
| **M4 经营与验收** | 测活预算/冷却/样本门槛、容量保留、告警 P1～P3、AC-01～32 验收矩阵、100～1000 QPS 压测 | 参数 3/11/12/16、FR-114 | 验收矩阵全绿；压测达标（P99≤50ms 决策开销） |

里程碑串行推进、每个可独立评审；M2 是风险最高的一段（流式接管），其 mock 场景直接复用 verify/ 的构造经验。

## 4. 文档索引

| 文档 | 内容 | 状态 |
| --- | --- | --- |
| 00（本篇） | 总览、硬约束、里程碑 | 草案 |
| [01 架构设计](./01-architecture.md) | 组件、请求路径、AxonHub 集成契约、部署 | 草案 |
| [02 数据模型](./02-data-model.md) | PG 单库 schema：资源注册、别名策略、价格版本、逐 Attempt 账本、订阅台账（双倍率）、健康/冷却、采集/余额、告警、保留分区 | 草案 |
| [03 GatewayAdapter 契约](./03-gateway-adapter.md) | 执行面接口 + AxonHub beta5 实现（Key-per-Channel、retry 置零、对账归并、隐藏重试补算）+ ccLoad 退路空壳 | 草案 |
| [04 采集器契约](./04-collector-adapter.md) | CollectorAdapter 接口 + 三家族（NewAPI/Sub2API/ASXS）字段映射 + 凭证生命周期状态机 | 草案 |
| [05 调度与经营策略](./05-scheduling-and-operations.md) | selector 候选过滤/排序/RoutePlan + steward 测活预算/冷却/订阅倾斜/容量保留/告警/错误预算 | 草案 |
| [06 部署与运维](./06-deployment-and-operations.md) | 单机 Compose 拓扑、AxonHub 集成部署、升级门禁（verify/ 准入）、备份保留、可观测、M0 部署清单 | 草案 |
