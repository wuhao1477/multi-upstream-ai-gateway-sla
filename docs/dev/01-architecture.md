# 架构设计（v0.2）

| 项目 | 内容 |
| --- | --- |
| 状态 | 草案，待评审 |
| 日期 | 2026-07-25（v0.2：按 [11 转向决策](./11-decision-full-selfbuilt.md) 移除外部网关，改为自研直连） |
| 栈 | Go（存储层 pgx + sqlc）/ PostgreSQL 单库 / 单机 Docker Compose / **无外部网关** |

## 1. 组件拓扑

```
                    ┌──────────────────── 单机 Docker Compose ────────────────────┐
客户端(Codex/OpenAI SDK) → Caddy LB → sla-core ×2 (Go) ──直连──→ ~20 上游渠道
                                        │                    (NewAPI/Sub2API 中转站, sk- key)
                                        ├── PostgreSQL（账本/台账/价格版本/健康状态，多实例共享）
                                        └── collector (Go, 同二进制子命令)
                                              └── NewAPI / Sub2API / ASXS 三家族采集器 → 上游站点管理面
```

- **sla-core**：同步请求路径 + 决策引擎 + **自研上游透传层** + 账本写入。无本地持久状态，实例无差别（FR-110 多实例）。
- **上游直连**：不再经任何外部网关（[11](./11-decision-full-selfbuilt.md)）。渠道与 Key 来自 PG 的 `channels`/`upstream_keys`（一期明文，FR-113）。
- **collector**：异步控制路径，承接 [ISSUE-002 适配器契约](../issues/ISSUE-002-collector-adapter-design.md#2-适配器契约)。

## 2. sla-core 内部模块

| 模块 | 职责 | 关键约束 |
| --- | --- | --- |
| `protocol` | OpenAI CC/Responses 端点、流式透传、工具/多模态字段原样转发 | FR-111；不落正文（FR-112） |
| `policy` | 模型别名→策略解析（SLA 等级、测活资格、优先序）；策略配置化+二次确认 | FR-062/115/117 |
| `selector` | 生成 RoutePlan：候选 Binding 序列 + 每跳期限。输入：价格版本、余额下限、健康/冷却、缓存作用域、订阅双倍率、容量保留、配额状态（unknown 默认排除，FR-118） | 决策 P99≤50ms → 全内存快照决策，PG 异步刷新 |
| `executor` | 按 RoutePlan 逐 Attempt 执行：**字节透传 + 旁路观察**→内容感知首字判定（排除 role-only/空 delta/注释心跳）→期限内未见有效首字则取消本跳并切下一 Binding；已提交有效内容后不再切换 | AC-31/32；判定逻辑可借鉴 AxonHub `hasResponseContent` 思路（[13 §2.1](./13-research-reassessment.md)） |
| `upstream` | **自研上游对接层**：请求转发、SSE 字节透传、取消传播、usage 提取、协议能力探测。详见 [03](./03-upstream-layer.md) | FR-111/119、AC-31/32 |
| `ledger` | Attempt 账本写入（异步批量落 PG）。**单一真相源，无对账环节**（转向后不再有外部网关账本需比对） | FR-097/098、FR-058 |
| `steward` | 测活预算/冷却/样本门槛、余额信号识别（参数 5 多策略）、告警 P1～P3 | 参数 3/11/16 |

## 3. 上游对接（自研直连）

详见 [03 上游对接层](./03-upstream-layer.md)。要点：

1. **字节级透传（硬约束）**：Responses/CC 响应体原样回送，不解析进领域模型再重发。依据 [13 §5](./13-research-reassessment.md)——LiteLLM / AxonHub / CLIProxyAPI 三个独立项目走解析-重组路线，全部在 Codex 兼容性上翻车。
2. **旁路观察**：内容感知 TTFT（AC-31）、usage 提取都在旁路做，不改变字节流。
3. **一次外部调用 = 一次上游调用**：不做隐藏重试；重试由 `executor` 按 RoutePlan 显式发起，各落一条 attempt（FR-119）。
4. **响应头透传**：`x-codex-*`（限流快照）、`x-models-etag`、`x-codex-turn-state`（会话粘性令牌）必须原样到达客户端（[13 §1.3](./13-research-reassessment.md)）。
5. **渠道与凭证**：直接读 PG 的 `channels`/`upstream_keys`，无需向外部网关开通（[02](./02-data-model.md)）。
6. **协议能力探测**：实测发现同一站点部分模型仅支持 Responses（CC 返 503），故按模型探测 `ProtocolSupport` 并参与候选过滤。

## 4. 请求路径时序（一次带接管的流式请求）

```
1 protocol 收请求(提取会话标识) → policy 解析别名 → selector 产出 RoutePlan [B1(期限5s), B2(期限8s)]
2 executor 用 B1 的渠道 Key 直连上游，字节流暂不提交给下游；旁路观察每个 SSE 事件
3 5s 内旁路未见 HasVisibleText（role-only/元事件/心跳不算）→ Close() 拆上游连接止损，切 B2
4 起 B2 → 旁路首次 HasVisibleText=true → 提交响应头+已缓冲字节 → 此后纯透传，不再切换
5 关单：Attempt#1(canceled_by_sla)、Attempt#2(committed) 落账；usage 取自旁路终帧
```

> 关键：第 2~4 步全程**字节级透传**，`executor` 只在旁路读事件元数据做判定，**不重组流**（[03 §3](./03-upstream-layer.md)）。

## 5. 数据面/控制面切分

- **同步路径只读内存快照**（价格、健康、余额下限、配额、订阅倍率），任何 PG 抖动不阻塞决策；快照由后台按参数 10 的频率刷新，数据过期按"越旧越保守"降级（FR-116）。
- **写路径全异步**：Attempt 账本、对账、采集结果批量写 PG；丢失容忍度：账本不可丢（同步 WAL），快照可重建。

## 6. 开放点（评审需拍板）

| # | 开放点 | 结论 |
| --- | --- | --- |
| 1 | 上游执行由谁承担 | ✅ **已定（2026-07-25）：自研直连，移除 AxonHub/ccLoad** —— 依据 [11 转向决策](./11-decision-full-selfbuilt.md)。数据面不再有外部网关单点 |
| 2 | Responses 协议如何处理 | ✅ **已定：字节级透传（硬约束）** —— 三个独立项目走解析-重组全部在 Codex 兼容性翻车（[13 §5](./13-research-reassessment.md)）；透传天然保真 35 字段与 reasoning item |
| 3 | 上游 Key 管理 | ✅ **已定**：直接读 PG `channels`/`upstream_keys`（一期明文，FR-113），无需向外部网关开通；经 [09 `/admin/bindings`](./09-admin-api.md) 登记 |
| 4 | 内容感知首字判定的实现 | ✅ **已定**：旁路观察 SSE 事件元数据判定（[03 §3.2](./03-upstream-layer.md)），不接管字节流；判定标准对齐 [ISSUE-001 假设3](../issues/ISSUE-001-tech-assumption-verification.md) 的三条铁证 |
