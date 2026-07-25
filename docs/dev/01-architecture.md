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
- **健康语义分层**：`/healthz`（LB 就绪，只看实例自身）≠ 渠道健康（selector 状态）≠ 全候选不可用（请求路径返回明确错误）。三者不可混用，详见 [06 §6](./06-deployment-and-operations.md)。

## 2. sla-core 内部模块

| 模块 | 职责 | 关键约束 |
| --- | --- | --- |
| `protocol` | OpenAI CC/Responses 端点、流式透传、工具/多模态字段原样转发 | FR-111；不落正文（FR-112） |
| `policy` | 模型别名→策略解析（SLA 等级、测活资格、优先序）；策略配置化+二次确认 | FR-062/115/117 |
| `selector` | 生成 RoutePlan：候选 Binding 序列 + 每跳期限。输入：价格版本、余额下限、健康/冷却、会话粘性、容量保留、配额状态（unknown 默认排除，FR-118）。⏭ 订阅双倍率移入二期（[15 §1.2](./15-scope-and-preflight.md)） | 决策 P99≤50ms → 全内存快照决策，PG 异步刷新 |
| `executor` | 按 RoutePlan 逐 Attempt 执行：**字节透传 + 旁路观察**→内容感知首字判定（排除 role-only/空 delta/注释心跳）→期限内未见有效首字则取消本跳并切下一 Binding；已提交有效内容后不再切换 | AC-31/32；判定逻辑可借鉴 AxonHub `hasResponseContent` 思路（[13 §2.1](./13-research-reassessment.md)） |
| `upstream` | **自研上游对接层**：请求转发、SSE 字节透传、取消传播、usage 提取、协议能力探测。详见 [03](./03-upstream-layer.md) | FR-111/119、AC-31/32 |
| `ledger` | Attempt 账本写入（异步批量落 PG）。**单一真相源，无对账环节**（转向后不再有外部网关账本需比对） | FR-097/098、FR-058 |
| `steward` | 冷却/样本门槛、余额信号识别（参数 5 多策略）、告警 P1～P3。⏭ 测活预算体系移二期（[15 S1](./15-scope-and-preflight.md)） | 参数 5/11/16 |

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
3 5s 内旁路未见 ShouldCommit（role-only/元事件/心跳不算；工具调用/拒答/空终态都算）→ Close() 拆上游连接止损，切 B2
4 起 B2 → 旁路首次 ShouldCommit=true → 提交响应头+已缓冲字节 → 此后纯透传，不再切换；TTFT 另由 HasTTFTOutput 打点（空响应则留 NULL）
5 关单：Attempt#1(canceled_by_sla)、Attempt#2(committed) 落账；usage 取自旁路终帧
```

> 关键：第 2~4 步全程**字节级透传**，`executor` 只在旁路读事件元数据做判定，**不重组流**（[03 §3](./03-upstream-layer.md)）。

## 5. 数据面/控制面切分

- **同步路径只读内存快照**（价格、健康、余额下限、配额、订阅倍率），任何 PG 抖动不阻塞决策；快照由后台按参数 10 的频率刷新，数据过期按"越旧越保守"降级（FR-116）。
- **写路径分两类，不可一概而论**（对抗性审查修正）：

  | 类别 | 写法 | 理由 |
  | --- | --- | --- |
  | **账本（request / attempt / usage）** | **不可全异步**，见 §5.1 持久化协议 | 转向自研后账本是**唯一真相源**（无外部网关账本可比对）。上游调用已真实产生费用，若记录还在内存队列时进程崩溃，成本/审计/错误预算**永久不可恢复** |
  | 采集结果、健康快照 | 异步批量写 PG | 可从上游重新采集或由账本重算，**丢失可重建** |

### 5.1 账本持久化协议（M1 契约，防崩溃丢账）

> **问题**：原设计写"账本不可丢（同步 WAL）"却又写"写路径全异步"——同步 WAL 只保护**已进入 PostgreSQL** 的事务，保护不了仍在内存队列中的记录。

| 时点 | 必须持久化的内容 | 同步/异步 |
| --- | --- | --- |
| **发起上游调用前** | `requests` 行 + 本跳 `attempts` 行（状态 `pending`，含 binding、price_version_id）+ `client_reservations` 预留（[02 §2bis](./02-data-model.md) D 阶段） | **同步提交**（先落库再发请求） |
| **首次 ShouldCommit → 放行响应头与缓冲字节之前** | `attempt_status='committed'`、`response_committed_at`、`has_ttft_output`、`content_aware_ttft_ms` | **同步提交（先落库再放行字节）** |
| **识别终帧 → 放行终帧字节之前** | `terminal_event`、usage、attempt_end | **同步提交（先落库再放行字节）**，[03 §3.0](./03-upstream-layer.md) |
| 每跳取消 / 中途状态 | cancel_reason、cancel_propagated | 异步，但经 **outbox** |
| 关单 | `final_status`、成本汇总 | 异步，但经 **outbox** |

> **"先落库再放行字节"为什么对首字也必须成立**（第 10 轮 [high]）：上一版只对终帧冻结了顺序，首字仍是"异步经 outbox"。于是进程可以在**首个内容字节已交给客户端、`committed` 尚未落库**时崩溃——库里还是 `pending`，恢复会判成 `unknown_billing`/`failed`，而用户**明明看到了半截输出**。这既违反 [AC-12 B 分支](./14-acceptance-matrix.md)（我们崩溃 → `interrupted`），也让流中断率漏计。
>
> **代价（明示）**：首字放行前多一次同步写，给 TTFT 增加约 1~2ms。**每请求只发生一次**（后续 delta 全部直通），相对金级 TTFT 预算（秒级）可忽略。这是用 2ms 换"客户端所见与账本所记不分裂"。

**三条不变式**：

1. **意图先行**：任何上游调用之前，其 `request`+`attempt` 意图**已在 PG 落库**。崩溃后可据 `pending` 记录判定"可能已产生费用"，不会静默丢账。
2. **幂等键**：状态更新以 `(attempt_id, 状态跃迁)` 为幂等键，重放不产生重复行、不覆盖更晚状态。
3. **outbox 先于内存队列**：异步更新先写 durable outbox（同库同事务），再由后台投递；进程崩溃后**重放 outbox**，不依赖内存队列存活。

**崩溃恢复终态契约**（与 [02 §4.2bis](./02-data-model.md) 租约扫描、[AC-35](./14-acceptance-matrix.md) 严格一致）：

| 崩溃时点 | 判据 | attempt | request | reservation（配额） | 告警 |
| --- | --- | --- | --- | --- | --- |
| ① 上游调用**前** | `pending` 且 `external_call_started_at IS NULL` | `failed` | `failed` | `abandoned`（释放） | 无 |
| ② 已发出、首字前 | `pending` 且 `external_call_started_at IS NOT NULL` | `unknown_billing` | `failed` | `settled`(估算)+待核对 | P2 |
| ③a 终帧已收、关单前 | `committed` 且 `terminal_event IS NOT NULL` | `completed`/`failed` | 同左 | `settled`(有实际用量则用实际) | 无/P3 |
| ③b 首字后、终帧前 | `committed` 且 `terminal_event IS NULL` | `interrupted` | `interrupted` | `settled`(估算)+待核对 | P3 |
| ④ 终帧后、关单前 | outbox 有 `usage`/`attempt_end` 事件 | `completed` | `completed` | `settled`(实际) | 无 |

**四条硬性要求**（[02 §4.2bis](./02-data-model.md) 是唯一实现规范）：

1. `pending` 与 `committed` **都是非终态**，恢复扫描必须同时覆盖——只扫 `pending` 会让 ③ 永久悬挂。
2. `executor` 在流式传输期间必须**每 ≤20s 续租** `lease_heartbeat_at`，否则长响应被误判崩溃。
3. attempt、request、reservation **在同一个 `finalize` 事务里一起终结**，不允许"账本终结了、配额还挂着"（配额永久泄漏 → 最终全部 429）。
4. **不可用 `attempt_status='committed'` 反推「见过首字」**——空终态/错误终态同样会 commit（[03 §3.2](./03-upstream-layer.md)）。③a/③b 必须读 `terminal_event`。终帧提交顺序另有约束：**先落 outbox 再放行终帧字节**（[03 §3.0](./03-upstream-layer.md)）。
5. `interrupted` 与 `failed` **对用户 SLA 的口径相同**（都是完整失败、都计入流中断率，FR-071/AC-12），区分终态只为归因（我们崩了 vs 上游断了）。
6. 四种情况都不得出现"请求完全不存在"、不得永久停留 `pending`/`committed`、不得残留 `reserved` 预留。

## 6. 开放点（评审需拍板）

| # | 开放点 | 结论 |
| --- | --- | --- |
| 1 | 上游执行由谁承担 | ✅ **已定（2026-07-25）：自研直连，移除 AxonHub/ccLoad** —— 依据 [11 转向决策](./11-decision-full-selfbuilt.md)。数据面不再有外部网关单点 |
| 2 | Responses 协议如何处理 | ✅ **已定：字节级透传（硬约束）** —— 三个独立项目走解析-重组全部在 Codex 兼容性翻车（[13 §5](./13-research-reassessment.md)）；透传天然保真 35 字段与 reasoning item |
| 3 | 上游 Key 管理 | ✅ **已定**：直接读 PG `channels`/`upstream_keys`（一期明文，FR-113），无需向外部网关开通；经 [09 `/admin/bindings`](./09-admin-api.md) 登记 |
| 4 | 内容感知首字判定的实现 | ✅ **已定**：旁路观察 SSE 事件元数据判定（[03 §3.2](./03-upstream-layer.md)），不接管字节流；判定标准对齐 [ISSUE-001 假设3](../issues/ISSUE-001-tech-assumption-verification.md) 的三条铁证 |
