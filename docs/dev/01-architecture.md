# 架构设计（v0.1 草案）

| 项目 | 内容 |
| --- | --- |
| 状态 | 草案，待评审 |
| 日期 | 2026-07-23 |
| 栈 | Go / PostgreSQL 单库 / 单机 Docker Compose / AxonHub `v1.0.0-beta5`（L0） |

## 1. 组件拓扑

```
                    ┌─────────────────────────────── 单机 Docker Compose ───────────────────────────────┐
客户端(OpenAI SDK) → Caddy LB → sla-core ×2 (Go) ──同步──→ AxonHub(beta5, stock) ──→ ~20 上游渠道
                                   │  │                        ↑ 管理 GraphQL(/admin/graphql)
                                   │  └──异步对账/管理────────────┘
                                   ├── PostgreSQL（账本/台账/价格版本/健康状态，多实例共享）
                                   └── collector (Go, 同二进制子命令或独立容器)
                                         └── NewAPI / Sub2API / ASXS 三家族采集器 → 上游站点管理面
```

- **sla-core**：同步请求路径 + 决策引擎 + 流式执行器 + 账本写入。无本地持久状态，实例无差别（FR-110 多实例）。
- **AxonHub**：受控执行面。单实例（见 §6 开放点 1），SQLite→建议改 PG DSN 与核心共库分 schema。
- **collector**：异步控制路径，承接 [ISSUE-002 适配器契约](../issues/ISSUE-002-collector-adapter-design.md#2-适配器契约)。

## 2. sla-core 内部模块

| 模块 | 职责 | 关键约束 |
| --- | --- | --- |
| `protocol` | OpenAI CC/Responses 端点、流式透传、工具/多模态字段原样转发 | FR-111；不落正文（FR-112） |
| `policy` | 模型别名→策略解析（SLA 等级、测活资格、优先序）；策略配置化+二次确认 | FR-062/115/117 |
| `selector` | 生成 RoutePlan：候选 Binding 序列 + 每跳期限。输入：价格版本、余额下限、健康/冷却、缓存作用域、订阅双倍率、容量保留、配额状态（unknown 默认排除，FR-118） | 决策 P99≤50ms → 全内存快照决策，PG 异步刷新 |
| `executor` | 按 RoutePlan 逐 Attempt 执行：SSE 预读→**内容感知首字判定**（排除 role-only/空 delta/注释心跳）→期限内未见有效首字则取消本跳并切下一 Binding；已提交有效内容后不再切换 | AC-31/32；借鉴 AxonHub `hasResponseContent` 与 ccLoad 延迟提交的实现思路 |
| `ledger` | Attempt 账本写入（异步批量落 PG）；对账器拉取 AxonHub `requests/executions/usageLogs` 按 externalID/时间窗关联；`errorMessage` 归并取消口径；**隐藏重试补算**（对 Codex 类渠道按网关行为修正上游调用数） | AC-30、FR-119、假设 2/4 契约 |
| `steward` | 测活预算/冷却/样本门槛、余额信号识别（参数 5 多策略）、告警 P1～P3 | 参数 3/11/16 |

## 3. AxonHub 集成契约（GatewayAdapter，运行时已验证的机制）

绑定机制用 ISSUE-001 假设 1 证实的 **Key-per-Channel**：

1. **每渠道预置一把 AxonHub API Key**，其 active profile 锁定该单渠道（`updateAPIKeyProfiles`，channelIDs 取 gid 末段数字）。执行某 Attempt = 用对应渠道的 Key 调 AxonHub `/v1/chat/completions`。
2. **AxonHub 自身重试置零**：`updateRetryPolicy{enabled:false}`——注意该 mutation 为**整体替换**（假设 2 补验发现），部署脚本必须全字段下发。否则默认在渠道内隐藏重试 3 次（假设 1 实测），会污染 Attempt 账本。
3. **对账**：经 `/admin/graphql` 拉 `requests(...)  { executions{...} usageLogs{...} }`；字段名按 [beta5 schema 适配表](../issues/ISSUE-001-tech-assumption-verification.md#beta5-schema-适配供今后升级重跑参考)。`metricsFirstTokenLatencyMs` 只存证不采信（AC-31）。
4. **渠道/定价管理**：建渠道后必须 `updateChannelStatus(enabled)`；模型价格经 `saveChannelModelPrices`（usage_per_unit，每 1M token）下发，使 usageLogs 产出 totalCost + costPriceReferenceID（假设 4 已证实链路）。
5. **订阅渠道**：claudecode/codex 等订阅 channel_type 由 AxonHub 原生承接；provider quota 状态经 `Channel.providerQuotaStatus` 采回，但**候选过滤在 selector 做**（FR-118，不依赖 AxonHub 的 enforcement，其默认关闭且 unknown 不排除）。
6. **退路**：GatewayAdapter 定义为接口；ccLoad 实现留空壳（假设 6 已验证其单资源拓扑可用），仅在 AxonHub 不可用预案启用。

## 4. 请求路径时序（一次带接管的流式请求）

```
1 protocol 收请求 → policy 解析别名 → selector 产出 RoutePlan [B1(期限5s), B2(期限8s)]
2 executor 用 B1 渠道 Key 调 AxonHub，SSE 预读不提交
3 5s 内未见有效首字（role-only/心跳不算）→ 取消 B1（上游连接拆除即止损，假设6已验证语义）
4 起 B2 → 收到首个含内容 delta → 提交响应头+缓冲 → 此后只透传，不再切换
5 关单：Attempt#1(canceled_by_sla)、Attempt#2(committed) 落账 → 对账器异步补 AxonHub 侧 execution/usage/cost
```

## 5. 数据面/控制面切分

- **同步路径只读内存快照**（价格、健康、余额下限、配额、订阅倍率），任何 PG 抖动不阻塞决策；快照由后台按参数 10 的频率刷新，数据过期按"越旧越保守"降级（FR-116）。
- **写路径全异步**：Attempt 账本、对账、采集结果批量写 PG；丢失容忍度：账本不可丢（同步 WAL），快照可重建。

## 6. 开放点（评审需拍板）

| # | 开放点 | 建议 |
| --- | --- | --- |
| 1 | **AxonHub 单实例是数据面单点**（FR-110 只约束自研核心） | ✅ **已实测**（[07 §2](./07-axonhub-runtime-probes.md)）：AxonHub 双实例 + 共享 PG **稳态双活可消除单点**；**约束**：迁移须串行（初始化/滚动升级单实例先跑，否则并发迁移崩实例）。一期推荐双实例，迁移串行写进 runbook |
| 2 | AxonHub 对 **OpenAI Responses 协议**的透传完整度 | ✅ **已实测**（[07 §3](./07-axonhub-runtime-probes.md)）：inbound `/v1/responses` 原生支持；outbound 须配 `openai_responses` 渠道（`openai` 型静默下转 CC）；为领域模型 round-trip（丢未知字段/重签 item id），标准字段够用无需自研转换，严格保真需 protocol 层直连（真实上游保真度待 M1 补验） |
| 3 | Key-per-Channel 的 **Key 数量管理**（20 渠道×可能的分组） | 由 GatewayAdapter 自动开通/回收并登记到 PG，禁止人工建 Key |
| 4 | AxonHub 存储从 SQLite 换 PG 共库 | ✅ **已实测**（[07 §1](./07-axonhub-runtime-probes.md)）：beta5 支持 PG，DSN `postgres://…?sslmode=disable`，`search_path` 与自研账本共库分 schema |
