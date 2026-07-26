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
| `ledger` | Attempt 账本写入：**关键事实同步直写 PG**（意图、首字、终帧/usage/结算——见 §5.1），仅 `downstream_*`/`cancel` 经 outbox 异步投递。**单一真相源，无对账环节** | FR-097/098、FR-058 |
| `steward` | 冷却/样本门槛、余额信号识别（参数 5 多策略）、告警 P1～P3、**渠道验证双轨**（canary + 主动测活及其预算体系，[05 §2](./05-scheduling-and-operations.md)） | 参数 3/5/6/11/16、FR-060~067 |

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
4 收尾 B1（closeout_attempt：终态+费用行+释放 canary claim）→ 插入 B2 的 attempt（dispatch_next）→ 才发起 B2
5 起 B2 → 旁路首次 ShouldCommit=true → 提交响应头+已缓冲字节 → 此后纯透传，不再切换；TTFT 另由 HasTTFTOutput 打点（空响应则留 NULL）
6 关单：Attempt#1(canceled_by_sla)、Attempt#2(completed) 落账；`actual_usd` = **两跳汇总**（[02](./02-data-model.md)）
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
| **鉴权通过、进入调度前** | `requests` 行（`final_status='pending'`，含 client、别名、协议） | **同步提交** |
| **发起上游调用前** | 本跳 `attempts` 行（`pending`，含 binding、price_version_id）+ `client_reservations` 预留（[02 §2bis](./02-data-model.md) D 阶段） | **同步提交**（先落库再发请求） |

> ⚠️ **`requests` 必须比 attempt/预留更早落库**（本轮自查）：原设计把三者绑在"发起上游调用前"同一步，于是
> **全候选不可用**（selector 输出空，无 attempt）与 **预留失败 429**（D 阶段返回 0 行）这两条路径**根本不会产生任何账本记录**——
> 与 FR-097/098「每个请求可查询」、AC-15「返回事件编号并落账本」、AC-33「429 可审计」全部冲突。
> 提前插入后，这两条路径都有 `pending` 行，再由 `finalize_abort` 推到 `unavailable`／`failed` 终态。
| **首次 ShouldCommit → 放行响应头与缓冲字节之前** | `attempt_status='committed'`、`response_committed_at`、`has_ttft_output`、`content_aware_ttft_ms` | **同步提交（先落库再放行字节）** |
| **识别终帧 → 放行终帧字节之前** | 见下方 `finalize_upstream` 行（**同步直写表，非 outbox**） | **同步提交（先落库再放行字节）**，[03 §3.0](./03-upstream-layer.md) |
| **每跳取消 / 每跳结束** | `attempt_status`(终态) + `cancel_reason` + `canceled_by_sla` | **同步直写 `attempts`** |
| 每跳取消后的纯观测字段 | `cancel_propagated` | 异步，经 **outbox** |
| **`finalize_upstream`**（终帧到达时） | **直写表**：`attempts.terminal_event` + `attempt_usage` + reservation 结算 + `attempt_status` 终态。**`requests.final_status` 保持 `pending`** | **同步**（不经 outbox），在放行终帧字节之前 |
| **`finalize_delivery`**（socket write 返回后） | `downstream_write_completed_at`，再据它推 `requests.final_status` | 异步，经 **outbox**（不涉及计费） |
| **`finalize_abort`**（取消／断流／内部超时／全候选不可用） | attempt 终态 + `cancel_reason` + **`requests.final_status`** + reservation 结算 + canary 释放 | **同步** |
| **`finalize_recovery`**（恢复扫描） | 同上，参数取自 [02 §4.2bis](./02-data-model.md) 分流表 | 后台任务 |

> **四个终结入口，只有 `finalize_upstream` 不写 `requests.final_status`**（它跑在字节放行前，交付未知；写了 K4 就不可达）。其余三者跑在"本请求已无后续"之时**必须写**，否则 request 永久 `pending`。`client_disconnect` → `canceled`，**不计入 SLA 失败**（PRD 术语）。

> ⚠️ **关单必须两阶段，不能一步到位**（第 12/13 轮 [critical]）：若在终帧到达时就把 `final_status` 写成 `completed`，那一刻字节**还没写给下游**；随后崩在 K4 窗口时 request 已是终态，恢复扫描的 `WHERE final_status='pending'` 闸门再也改不动它 —— **③c/K4 分支被正常路径整个绕过**。完整 SQL 与幂等约束见 [02 §2bis](./02-data-model.md)。

> **"先落库再放行字节"为什么对首字也必须成立**（第 10 轮 [high]）：上一版只对终帧冻结了顺序，首字仍是"异步经 outbox"。于是进程可以在**首个内容字节已交给客户端、`committed` 尚未落库**时崩溃——库里还是 `pending`，恢复会判成 `unknown_billing`/`failed`，而用户**明明看到了半截输出**。这既违反 [AC-12 B 分支](./14-acceptance-matrix.md)（我们崩溃 → `interrupted`），也让流中断率漏计。
>
> **代价（明示，且是待验证假设）**：首字放行前多一次同步写。**每请求只发生一次**（后续 delta 全部直通）。
> ⚠️ "约 1~2ms"目前**没有负载条件与 PG 分位依据**，不得当结论用——1000 QPS 下首字+终帧合计 ≈2000 次同步事务/秒，连接池排队完全可能把它推高一个量级。
> **约束**：① [AC-34/36](./14-acceptance-matrix.md) 须用 `downstream_ttft_delay_ms` / `downstream_finish_delay_ms` 两个**专用指标**度量（`总延迟 − 上游耗时` 会把它抵消掉，测不出来）；② 实测阈值由 M4 压测冻结并写回本节；③ 允许**组提交**（≤N 请求合并一个事务、linger ≤2ms）——正确性只要求"字节放行前已提交"，不要求每请求一事务。

**三条不变式**：

1. **意图先行**：任何上游调用之前，其 `request`+`attempt` 意图**已在 PG 落库**。崩溃后可据 `pending` 记录判定"可能已产生费用"，不会静默丢账。
2. **幂等键**：状态更新以 `(attempt_id, 状态跃迁)` 为幂等键，重放不产生重复行、不覆盖更晚状态。
3. **outbox 只承载异步事实**：需要「先落库再放行字节」的（首字、终帧）一律**同步直写表**；outbox 只留 `downstream_first_byte`/`downstream_write_completed`/`cancel`。**同一事实不得有两条写入路径**（[02 §2bis](./02-data-model.md) 写入路径分工）。
4. **outbox 先于内存队列**：异步更新先写 durable outbox（同库同事务），再由后台投递；进程崩溃后**重放 outbox**，不依赖内存队列存活。

**崩溃恢复终态契约**（与 [02 §4.2bis](./02-data-model.md) 租约扫描、[AC-35](./14-acceptance-matrix.md) 严格一致）：

| # | 崩溃时点 | 判据 | attempt | request | reservation | 告警 |
| --- | --- | --- | --- | --- | --- | --- |
| ① | 上游调用**前** | `external_call_started_at IS NULL` | `failed` | `failed` | `abandoned`（释放） | 无 |
| ② | 已发出、首字未落库（K1） | `response_committed_at IS NULL` | `unknown_billing` | `failed` | `settled`(汇总)+待核对 | P2 |
| ③b1 | 首字已落库、**写出未确认**（K2） | `terminal_event IS NULL` 且 `downstream_first_byte_written_at IS NULL` | `interrupted` | **`failed`** | `settled`(汇总)+待核对 | P3 |
| ③b2 | 已确认写出、终帧前（K3） | `terminal_event IS NULL` 且 `downstream_first_byte_written_at NOT NULL` | `interrupted` | `interrupted` | `settled`(汇总)+待核对 | P3 |
| ③c | 终帧已落库、**写完未确认**（K4） | `terminal_event NOT NULL` 且 `downstream_write_completed_at IS NULL` | 由 `terminal_event` 定 | **`interrupted`** | `settled`(**实际**) | 无 |

> **「全部写完」不是恢复分支**：`finalize_delivery` 原子地同时写列与 `final_status`，故不存在「列已非空、request 仍 pending」的状态。已写完的请求由**启动时先 drain outbox** 收口。

**四条硬性要求**（[02 §4.2bis](./02-data-model.md) 是唯一实现规范）：

1. **恢复扫描是 `request` 级的**：唯一非终态是 `requests.final_status='pending'`。**不可扫 `attempt_status`**——两阶段关单后 attempt 先进终态、request 后进终态，扫 attempt 会让 K4 永远扫不到（[02 §4.2bis](./02-data-model.md)）。执行顺序冻结为：**先 drain outbox → 再恢复扫描**。
2. `executor` 在流式传输期间必须**每 ≤20s 续租** `lease_heartbeat_at`，否则长响应被误判崩溃。
3. **reservation 与 attempt 在 `finalize_upstream` 同一事务里终结**，不允许"账本终结了、配额还挂着"（配额永久泄漏 → 最终全部 429）。**`requests.final_status` 不在该事务内**——它由 `finalize_delivery` 依据写出事实推定（见上表）。
4. **DB 提交与 socket 写出无法原子化，反方向窗口不可消除**：先落库再放行只挡住"客户端有、库里没有"；"库里有、客户端没收全"必然存在（③c）。故 `attempts` 另记 `downstream_first_byte_written_at`/`downstream_write_completed_at` 两个**独立事实**，冲突时按"未确认 = 未交付"取保守解释——attempt 层可以是 `completed`（成本精确已知），request 层仍判 `interrupted`（交付未确认）。详见 [03 §3.0](./03-upstream-layer.md) 四时点表。
5. **不可用 `attempt_status='committed'` 反推「见过首字」**——空终态/错误终态同样会 commit（[03 §3.2](./03-upstream-layer.md)）。③b／③c 的区分必须读 `terminal_event`。首字与终帧的提交顺序另有约束：**先同步直写表、再放行字节**（[03 §3.0](./03-upstream-layer.md)、[02 §2bis 写入路径分工](./02-data-model.md)）。
6. **`interrupted` 与 `failed` 都计入用户 SLA 失败**（FR-071），但**流中断率的口径更窄**：
   ```
   计入流中断 ⟺ attempts.stream_broken = true
              OR ( requests.final_status = 'interrupted'
                   AND attempts.downstream_first_byte_written_at IS NOT NULL )
   分母 = 同窗口内 requests.is_streaming = true 的全部请求
   ```
   正常成功（`completed`）**不计**；K2（`failed`）**不计**；K3／K4 **计**；上游断流（`stream_broken=true`）**计**。
   唯一口径见 [02 §4.2bis](./02-data-model.md)，本节不得另立简写。
7. 四种情况都不得出现"请求完全不存在"、不得永久停留 `pending`/`committed`、不得残留 `reserved` 预留。

## 6. 开放点（评审需拍板）

| # | 开放点 | 结论 |
| --- | --- | --- |
| 1 | 上游执行由谁承担 | ✅ **已定（2026-07-25）：自研直连，移除 AxonHub/ccLoad** —— 依据 [11 转向决策](./11-decision-full-selfbuilt.md)。数据面不再有外部网关单点 |
| 2 | Responses 协议如何处理 | ✅ **已定：字节级透传（硬约束）** —— 三个独立项目走解析-重组全部在 Codex 兼容性翻车（[13 §5](./13-research-reassessment.md)）；透传天然保真 35 字段与 reasoning item |
| 3 | 上游 Key 管理 | ✅ **已定**：直接读 PG `channels`/`upstream_keys`（一期明文，FR-113），无需向外部网关开通；经 [09 `/admin/bindings`](./09-admin-api.md) 登记 |
| 4 | 内容感知首字判定的实现 | ✅ **已定**：旁路观察 SSE 事件元数据判定（[03 §3.2](./03-upstream-layer.md)），不接管字节流；判定标准对齐 [ISSUE-001 假设3](../issues/ISSUE-001-tech-assumption-verification.md) 的三条铁证 |
