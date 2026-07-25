# 14 验收矩阵（AC → 里程碑 → 判定方法 · v0.1）

| 项目 | 内容 |
| --- | --- |
| 状态 | 草案，待评审（**M1 开工前须定稿**） |
| 日期 | 2026-07-25 |
| 缘起 | PM 视角审查发现：AC 未与里程碑挂钩，[00 M4](./00-overview-and-milestones.md) 写着"验收矩阵全绿"但**该矩阵不存在** → 每个里程碑无法闭环验收 |
| 作用 | 把 [PRD](../PRD.md) 的 **AC-01～36 全集**逐条绑定到里程碑、验证环境、判定阈值。**每条 AC 恰好归属一个分期与一个里程碑**（见 §1 总览）。开发说"做完了"时，PM 据本表判定真假 |
| 分期依据 | **一期/二期划分以 [PRD §2.1](../PRD.md) 为准**，本表由其派生 |
| 原则 | 每条 AC 必须有：① 归属里程碑 ② 可执行的验证方式 ③ **不含"基本正常""大致达标"这类不可判定措辞** |

---

## 0. 验证环境约定

| 环境 | 用途 | 说明 |
| --- | --- | --- |
| **MOCK** | 可重复、零成本、CI 常跑 | `verify/mock_upstream.py` 场景集（role-only / 空 SSE / 慢首帧 / 心跳 / 500 / abort），[11 §3](./11-decision-full-selfbuilt.md) |
| **REAL** | 真实上游，里程碑验收时手动跑 | NewAPI/Sub2API 中转站真实 `sk-` key（[07 §3bis](./07-axonhub-runtime-probes.md) 已有基线） |
| **FIXTURE** | 构造数据 + 单元/集成测试 | 账本、台账、价格版本等纯数据逻辑 |
| **LOAD** | 压测 | 100～1000 QPS（FR-114） |

---

## 1. 里程碑归属总览

| 里程碑 | 验收 AC | 条数 |
| --- | --- | --- |
| **M0 骨架** | AC-27、**AC-33**（入站鉴权） | 2 |
| **M1 上游直连 + 账本** | AC-01、AC-16、AC-26、AC-30、AC-31、AC-32、**AC-35**（崩溃恢复） | 7 |
| **M2 流式 SLA 核心** | AC-06、AC-12、AC-15、AC-25 | 4 |
| **M3 元数据采集** | AC-02、AC-03、AC-04、AC-05、AC-17、AC-19、AC-28、AC-29 | 8 |
| **M4 经营与验收** | AC-08、AC-11、AC-13、AC-14、AC-18、**AC-34**（1000 并发抗压）、**AC-36**（1000 QPS 吞吐） | 7 |
| 合计（一期） | | **28** |
| ⏭ **二期（订阅制）** | AC-20、AC-21、AC-22、AC-23、AC-24 | 5 |
| ⏭ **二期（主动测活，[15 S1](./15-scope-and-preflight.md)）** | AC-09、AC-10 | 2 |
| ⏭ **二期（缓存进阶 FR-056）** | AC-07 | 1 |

---

## 2. 逐条验收定义

### M0 骨架

| AC | 场景 | 环境 | 判定方法（可执行） |
| --- | --- | --- | --- |
| AC-27 | 自研核心某一实例宕机 | FIXTURE | `docker stop sla-core-a` → 持续打 30 个请求，**全部成功、无一失败**；Caddy 日志显示已摘除该实例；恢复后自动重新纳入 |
| **AC-33**（新增） | 入站鉴权：无凭证 / 已吊销 / 越权别名 / 超配额 | FIXTURE | ① 不带 `Authorization` → **401**；② 用已 `revoked` 凭证 → **401**；③ 用 `allowed_aliases` 外的别名 → **403**；④ 超 `rpm_limit` → **429**；⑤ 合法凭证正常 200。**跨实例并发**：⑥ 双 core 同时打，日费用累计到 `quota_daily_usd` 时**必然被拒**（不得因预留晚于检查而超限）；⑦ RPM 窗口跨实例共享（在 A 打满后 B 也拒）；⑧ core 重启后配额计数不清零。
**预留正确性**（[02 §2bis](./02-data-model.md) 上界算法）：⑨ 预留额 = RoutePlan 全部跳的上界之和（发生接管时不超预留）；⑩ **实际用量高于初始估算**时结算按实际写入，且溢出后**下一个请求必被 429**；⑪ `models.max_input_tokens`/`max_output_tokens` 为 NULL 的模型，其全部 binding **不得进入候选**（无上界即不可预留）；且 `POST /admin/models` 缺这两个字段必须返回 **400**，建成后 selector 能正确读到两列；⑫ **事务幂等重试**（结算已不经 outbox，[02 §2bis 写入路径分工](./02-data-model.md)）：同一 `settle_event_key` 重放 `finalize_upstream`、同一 `reservation_adjustments.event_key` 重放 `adjust`，`settled_usd` **不翻倍**、`reserved_usd` **不为负**；连接中断后重试同样只生效一次；⑬ 高并发（100 并发同凭证）下 `reserved_usd + settled_usd` **恒不超** `quota_daily_usd` + 单请求上界；⑯ **多模态/续接请求**（含 `image_url`、`previous_response_id` 等标记）按 `models.max_input_tokens` 预留，且 `max_input_tokens`/`max_output_tokens` 为 NULL 的模型 binding 不进候选；⑰ RPM 与日配额的**原子语句各自返回 0 行时必须 429**，不得先放行再补记；㉒ **`rpm_limit IS NULL` 的凭证连发 100 请求全部通过**（不限速语义，须整段跳过 RPM 闸，不得因 `count < NULL` 恒 UNKNOWN 而从第二个请求起误 429）；㉓ 溢出后的拒绝**由 `reserved_usd + settled_usd >= quota_daily_usd` 派生**，库中**不存在** `over_quota` 列，断言不得引用它。
**人工修正**（[02 §2bis](./02-data-model.md) `adjust` 事务）：⑭ 恢复扫描产生的 `needs_manual_review` 记录能被 `adjust` 修正为真实值且标志清除；⑮ 同一 `event_key` 重复提交、或事务提交后客户端未收到响应而重试，`settled_usd` **不得二次变动**；⑱ **两个不同 `event_key` 并发修正同一 reservation**，收敛后 `client_daily_spend.settled_usd` 与 `client_reservations.actual_usd` **必须一致**（不得出现 10→12/13 并发后聚合变 15 的失真）；⑲ `adjust` 只能改动该 reservation 自身的 client 与日期（参数不可指定）；⑳ **负值拒绝**：`new_actual_usd < 0` 返回 400、事务回滚、`settled_usd` 不变，且修正后 `settled_usd` 恒 ≥ 0；㉑ **`finalize` 同样不接受 client/日期/预留额传参**——跨午夜长流（预留在 D 日、结算在 D+1 日）的费用必须记在 **D 日**，且 `reserved_usd` 在 D 日归零。
另断言：`GET /admin/clients` **不回显完整凭证**、库中 `secret_hash` 非明文、日志中不出现凭证明文 |

### M1 上游直连 + 账本

| AC | 场景 | 环境 | 判定方法（可执行） |
| --- | --- | --- | --- |
| AC-01 | 配置 20 渠道/账号/Key/模型/故障域 | FIXTURE | 经 `/admin/bindings` 批量登记 20 渠道；`GET /admin/bindings` 返回 20 条且字段完整；`bindings` 表行数 = 20 |
| AC-16 | 查询任一历史请求 | FIXTURE | 任取一个 `request_id`，`GET /admin/ledger/requests/{id}` 返回：全部 attempt、每跳 binding、状态、TTFT、usage、成本、决策快照；**字段无 NULL 缺失**（失败/取消的 usage 为空属预期） |
| AC-26 | CC 与 Responses 两种协议、流式 + 工具调用 | **REAL** | 两协议各发 1 次流式 + 1 次带工具调用请求；**4 次全部 200**；工具调用字段在响应中**原样存在**（对比直连基线 diff 为空） |
| AC-30 | 客户端断开 / 上游断流 / 内部超时三种中断 | MOCK | 三场景各构造 1 次；`attempts.cancel_reason` 分别为 `client_disconnect` / `upstream_disconnect` / `internal_timeout`；三者 `request_id` 关联正确 |
| AC-31 | role-only 元事件后 0.5s 才发首内容 | MOCK | **七场景 × 双断言**（[03 §3.2](./03-upstream-layer.md) 拆为 `ShouldCommit`/`HasTTFTOutput`，须分别断言）：<br>`mock-normal` → commit=true@首 delta，ttft≈500ms<br>`mock-heartbeat` → 心跳不触发，ttft≈600ms+<br>`mock-tool-only` → commit=true@首 `function_call`/`tool_calls`，ttft=该时刻，**不得被接管取消**<br>`mock-refusal-only` → 同上<br>`mock-reasoning-summary-only` → 同上<br>`mock-empty-sse`（`response.completed` 无 delta）→ **commit=true（不取消）但 ttft=NULL**<br>`mock-error-terminal` → commit=true（按失败关单）、ttft=NULL<br>全场景**不得**出现 ttft≈0ms |
| AC-32 | 已输出首字后中途取消 | MOCK | 客户端收 3 chunk 后断开 → mock 侧下一次写入 **EPIPE**、停止产出；`attempts.cancel_propagated = true`；上游连接关闭时刻 − 客户端断开时刻 **< 1s** |

| **AC-35** | 崩溃恢复：账本 + 配额 + 交付终态 | FIXTURE | 自动化在下列时点 `kill -9` core 并重启，**每个时点断言五项**：`attempt_status` / `requests.final_status` / `client_reservations.state` / `client_daily_spend.reserved_usd` / 下游实际收到的字节。判定表与 [03 §3.0](./03-upstream-layer.md) K1~K4、[02 §4.2bis](./02-data-model.md) ①／②／③b1／③b2／③c **逐行一致**：<br>① 调用前 → `failed`/`failed`/`abandoned`/预留归零<br>② 已发出未见首字（K1）→ `unknown_billing`/`failed`/`settled`+待核对+P2<br>③b1 首字已落库但**写出未确认**（K2）→ `interrupted`/**`failed`**（**不计流中断**；不断言下游物理零字节）<br>③b2 已写出部分、终帧前（K3）→ `interrupted`/`interrupted`（**计流中断**）<br>③c 终帧已落库但**字节未写完**（K4）→ attempt **由 `terminal_event` 决定**（error/incomplete 须为 `failed`，**不得**记成渠道成功）/ request `interrupted` / `settled` 用**实际用量**、无需人工核对<br>**「全部写完」不是恢复分支**——`finalize_delivery` 原子写列+终态，故不存在「列已非空、request 仍 pending」；该情形由**启动时先 drain outbox** 收口。须补用例："socket write 已返回、`downstream_write_completed` outbox 已写但未投递" 时 kill，重启后经 outbox 重放得到 `completed`（**不得**被恢复扫描误判为 ③c/`interrupted`）。<br>**K4 必须单独注入**（`finalize_upstream` 事务提交返回之后、socket write 之前 kill）——DB 提交与 socket 写出无法原子化，这个窗口**不可消除**，不得假设它不存在。<br>**共同断言**：无 `pending`/`committed` 残留、无 `reserved` 预留残留、跨午夜请求费用记在**预留那一天**；`ledger_outbox` 重放不产生重复效果（现枚举只含 `downstream_first_byte`/`downstream_write_completed`/`cancel`，**结算不经 outbox**，其幂等由 `settle_event_key` 保证）。<br>另须含**长流式续租用例**：90s 流式响应在续租正常时**不得**被恢复扫描终结 |

> **M1 关键验收**（[00](./00-overview-and-milestones.md) 已列）：REAL 环境 Responses 响应与直连基线逐字段 diff，**35 字段与 reasoning item 零丢失**。

### M2 流式 SLA 核心

| AC | 场景 | 环境 | 判定方法（可执行） |
| --- | --- | --- | --- |
| AC-06 | A 渠道 ~20s 低价、B 渠道 ~3s 较贵 | MOCK | 金级别名请求：首跳选 A，期限内未见有效首字 → 切 B；**最终 TTFT < 该等级目标**；账本含 2 条 attempt（#1 `canceled_by_sla`、#2 `committed`） |
| AC-12 | 已输出首字后流中断（**两个分支，终态不同但 SLA 口径相同**） | MOCK | **A 上游断流（我们存活、观测得到）**：`mock-abort` → `attempts.stream_broken=true`、`attempt_status='failed'`、`requests.final_status='failed'`。**B 网关自身崩溃（无观测）**：**无统一终态**——按下方四个 kill 时点分别判定（[02 §4.2bis](./02-data-model.md) ②/③b1/③b2/③c）。<br>**B 分支（网关自身崩溃）须覆盖 [03 §3.0](./03-upstream-layer.md) 的**四个 kill 时点**，每个都断言 attempt 与 request 两层终态：K1 首字同步写提交前 → `unknown_billing`/`failed`；K2 首字已提交但**写出未确认** → `interrupted`/`failed`（**不**计流中断）；K3 `finalize_upstream` 提交前（已确认写出过首字节）→ `interrupted`/`interrupted`（计流中断）；**K4 `finalize_upstream` 已提交但字节未写出** → attempt **由 `terminal_event` 决定**（`completed`/`empty_completed` → `completed`；`error`/`incomplete` → **`failed`**，**不得**把上游失败记成渠道成功）、成本用实际用量无需人工核对；request `interrupted`（写出未确认 → 保守判失败、计流中断）。<br>**K4 必须单独注入**（在 `finalize_upstream` 提交返回之后、socket write 之前 kill）——这是 DB 提交与 socket 写出无法原子化导致的**不可消除**窗口，不得假设它不存在。<br>⚠️ **断言必须分两组**：一组断言**数据库终态**，另一组断言**下游实际观测到的字节**。两列是 write 返回后异步落库的，`IS NULL` 只代表「未确认写出」——**不得由数据库 NULL 推导物理零字节**（[03 §3.0](./03-upstream-layer.md)）。K2 用例只断言「数据库判 `failed` 且不计流中断」，不断言下游必然零字节。<br>**两分支共同断言**：①**不得**拼接第二个响应（下游字节在中断处结束）；②都计入用户 SLA **失败**（FR-071）；`stream_break_rate` 按**唯一判据**算——`stream_broken=true OR (final_status='interrupted' AND downstream_first_byte_written_at IS NOT NULL)`，故 A 与 K3/K4 计入、**K2 不计入**；另须断言**正常成功的流式请求一律不计入**（[02 §4.2bis](./02-data-model.md)）；③已记录的 `content_aware_ttft_ms` 照常保留计入。<br>`interrupted` 是**可区分的存储终态**而非另一种成功——它存在的唯一理由是把"我们崩了"与"上游断了"在排障与渠道健康归因上分开（B 不计入该 binding 成功率，A 计入） |
| AC-15 | 全部合规资源不可用 | FIXTURE | 所有候选置不可用：金级排队 ≤15s 后返回明确错误（含建议重试时间 + 事件编号）；**响应中无任何上游内容**；未旁路未降级模型 |
| AC-25 | `gpt-5.5` vs `gpt-5.5-sla-1` 两个别名 | FIXTURE | 各发 100 次：两者**适用各自别名映射的策略**（一期为单级 SLA + 测活资格标记）；⏭ 测活分配行为随主动测活移入二期验证 |

### M3 元数据采集

> ⏭ **订阅相关 AC-20/21/22/23/24 已移入二期**（[15 §1.2](./15-scope-and-preflight.md)），下表不含。

| AC | 场景 | 环境 | 判定方法（可执行） |
| --- | --- | --- | --- |
| AC-02 | 某渠道模型价格上涨 | FIXTURE | 写入新价格版本 → 该渠道新流量受限；**历史请求按其 `price_version_id` 复算成本不变** |
| AC-03 | 价格下降但未验证 | FIXTURE | 降价版本 `confirmed=false` 时流量**不**立即增加；确认后逐步增加（单次变化幅度 ≤ 配置上限） |
| AC-04 | 多 Key 共享账号余额且并发 | FIXTURE | 同 `balance_group_key` 的多 Key 并发：余额**只计一次**，不重复累加可用额度 |
| AC-05 | 账号有余额但 Key 日额度耗尽 | FIXTURE | 该 Key 被排除出候选（决策快照原因 = Key 额度耗尽），同账号其他 Key 仍可用 |
| AC-17 | 两渠道不同币种/计费单位 | FIXTURE | 统一美元口径比较（一期 1:1）；不同计费单位归一后可比 |
| AC-19 | 余额耗尽 / Key 失效 / 错误预算快速消耗 | FIXTURE | 三场景各产生 P1 或 P2 `alert_events`；**P1 无延迟**（触发到落库 < 5s）；同因合并为一个持续事件 |
| AC-28 | 三家族站点（NewAPI/Sub2API/闭源）接入采集 | **REAL** | 4 个实测站点：`Detect()` 正确归族；各自 `Capabilities()` 与 [04 §3.4](./04-collector-adapter.md) 矩阵一致；不支持字段返回 `unsupported` 而非留空 |
| AC-29 | 非标准方式返回"余额不足" | **REAL** | 构造/捕获该信号 → `balance_signals.balance_state = 'exhausted'`，停止向其发新付费请求 |

### M4 经营与验收

| AC | 场景 | 环境 | 判定方法（可执行） |
| --- | --- | --- | --- |
| AC-08 | 低权重渠道长期无样本 → 受控验证闭环（**FR-121**） | FIXTURE | ①样本不足（1h<20 且 24h<100）→ `low_confidence=true`，**不作稳定主渠道**；②**新建 binding** 与**冷却期满 binding** 都进入 `canary`，在**不制造任何全局故障**的前提下，仅靠正常业务流量走完 `canary → observing → available`（[05 §2.0](./05-scheduling-and-operations.md)）；③canary 硬上限生效：单 binding 每小时 ≤`canary_max_per_hour`、并发 ≤`canary_max_concurrent`，**金/银别名请求一次都不得**落到 canary binding；④canary 连续失败达阈值 → 退回 `cooling` 且退避翻倍；⑤无健康接管候选时**不分配** canary；⑥**双 core 高竞态**：两实例同时对同一 canary binding 打 200 请求，实际落到该 binding 的并发**恒 ≤`canary_max_concurrent`**、每小时**恒 ≤`canary_max_per_hour`**（原子 claim 生效，[05 §2.0](./05-scheduling-and-operations.md)）；⑦**claim 与 attempt 同事务**：在 claim 成功、attempt 未落库之间 kill，重启后 `canary_inflight` 不泄漏且 `canary_claims` 无孤儿行；⑧请求结束/租约过期回收后 `canary_inflight` 归零，且回收**不得**误伤尚在事务中的 claimant；⑨**长流不被误回收**：一个持续 >5 分钟的 canary 流式请求期间，claim 随 attempt 心跳持续续租、不被后台回收，并发上限全程不被突破 |
| AC-11 | 多渠道共享同一官方上游并同时故障 | FIXTURE | 同 `fault_domain` 的资源被整体降权/排除，**不逐个重试**；告警标注故障域 |
| AC-13 | 请求含不可重复的外部写入 | FIXTURE | 该请求**不被分配测活**、**不并发重试**（参数 6 默认全局禁并发重试） |
| AC-14 | 渠道不满足租户数据许可但价格最低 | FIXTURE | ①开关默认 `false` 时该渠道正常入选（一期表现）；②置 `data_policy_enabled=true` 并录一条 `effect='deny'` 规则后，`POST /admin/data-policies/simulate` 与真实请求的 `decision_snapshot.excluded[]` **都**须给出该渠道 + 原因 `data_policy_denied`，且**排除发生在价格排序前**（断言最低价渠道未被选中）。③**伪造 `X-Data-Class: public` 请求头不得改变求值结果**（属性只认 `gateway_clients` 行）。载体：[02 §2ter](./02-data-model.md)、[09 §5](./09-admin-api.md) |
| AC-18 | 单租户流量突增（容量隔离，一期无主动测活） | FIXTURE | 单租户突增时**不占用接管保留容量**；受限流量按配置规则处理（排队/拒绝），其他租户不受影响 |
| **AC-36** | **峰值吞吐 1000 QPS**（FR-114 吞吐门禁） | **LOAD** | 按 §2bis 峰值阶段：**1000 QPS 持续 60 秒**（mock 上游、90% 流式），决策开销 **P99 ≤50ms**、错误率 **<0.1%**、无 OOM/FD 耗尽。⚠️ 与 AC-34 是**两个独立门禁**：并发门禁压「同时在线数」，吞吐门禁压「每秒请求数」，二者都必须过 |
| **AC-34** | **1000 并发用户高强度持续使用**（程序自身抗压） | **LOAD** | 按 §2bis 冻结模型：① 1000 并发稳态 30 分钟，决策开销 **P99 ≤50ms**、错误率 <0.01%；② 内存不持续增长、结束后 goroutine/连接回落基线 ±10%、无 OOM/FD 耗尽；③ **另须完成上限探测并记录容量拐点** |

### ⏭ 二期（缓存进阶，FR-056）

> 一期只做**会话粘性**（连续会话优先沿用上一轮渠道），不做缓存损失预测与缓存率目标约束（[PRD §2.1](../PRD.md)）。故本条随 FR-056 移入二期。

| AC | 场景 | 环境 | 判定方法（可执行） |
| --- | --- | --- | --- |
| AC-07 | 切低价渠道会使缓存率低于目标 | FIXTURE | 构造缓存作用域不可延续的候选：`selector` **不选**该候选（决策快照 `excluded` 含缓存原因）；除非更高等级 SLA 要求接管 |

### ⏭ 二期（主动测活，[15 §2.1 S1](./15-scope-and-preflight.md)）

| AC | 场景 | 环境 | 判定方法（可执行） |
| --- | --- | --- | --- |
| AC-09 | 测活渠道首字超时 | MOCK | 测活请求分配到慢渠道 → 期限到即接管；**用户侧 TTFT 仍达标**；该次测活失败计入探索预算而非用户错误预算 |
| AC-10 | 测活失败或费用超标 | FIXTURE | 达任一预算上限即**暂停新测活**；正常流量**不受影响**（对比暂停前后主流量成功率差 < 1%） |

### ⏭ 二期（订阅制适配，[15 §1.2](./15-scope-and-preflight.md)）

> 以下 5 条随订阅制一并推迟；判定方法已定，二期直接可用。

| AC | 场景 | 环境 | 判定方法（可执行） |
| --- | --- | --- | --- |
| AC-20 | 订阅临近到期且预计较多未用额度 | FIXTURE | 三道闸全满足时才倾斜（成本 ≤1.5×、流量 ≤40%、剩余 ≤7 天且浪费 ≥20%）；`subscription_waste_forecast` 有预测值 |
| AC-21 | 订阅临近到期但倍率高/性能差 | FIXTURE | **不倾斜**，允许到期；`unusable_reason` 记录原因 |
| AC-22 | 多账号/Key 共享同一订阅额度 | **REAL** | Sub2API 站点按 `(ext_user_id, group_id)` 归集：剩余额度**只计一次**，不按 Key 重复计算 |
| AC-23 | 增加订阅流量将触发超额或突破成本上限 | FIXTURE | 停止增加该渠道流量；保留真实成本记录；不以提高消耗率为由继续 |
| AC-24 | 用满 0.034 订阅 vs 倍率 0.035 普通 | FIXTURE | **调度选订阅渠道**（用满倍率排序）；**账务报表按实际倍率 0.045 记账**——两个口径同时正确 |

## 2bis. AC-18 负载模型（**已冻结，开工前定**）

> **核心原则（2026-07-25 负责人确认）**：**压的是程序本身，不是上游。**
> 上游可以无限扩充（加渠道、加 Key、加账号），但**自研核心本身扛不住的话，多少上游都没意义**。因此压测一律用 **mock 上游**并把上游耗时从判定中剔除——本项验收衡量的是"这套程序的天花板在哪"，越高越好。
>
> 对抗性审查另指出：只写"100~1000 QPS"无法判定——同样 QPS 可以打得很轻也可以很重，团队能用低压力模型"通过"P99≤50ms 而 PM 无法复现。故冻结为**唯一可复现配置**。

### 最低标准：1000 并发用户高强度使用

| 参数 | 冻结值 | 依据 |
| --- | --- | --- |
| **并发用户** | **1000 个并发会话持续活跃** | 负责人定的最低门槛："能扛住 1k 人同时高强度使用" |
| **单用户强度** | 每用户持续发流式请求，一轮结束立即发下一轮（**无思考间隔**） | "高强度"= 不留空闲，逼出连接与 goroutine 峰值 |
| **由此得到的 QPS** | 视单请求时长自然产生（mock 上游约 2s/请求 → **约 500 QPS 稳态**）；**QPS 是结果不是输入** | 真实压力来自"1000 条并发流同时占用资源"，而非请求速率 |
| **预热** | 2 分钟，结果不计入统计 | 排除连接池/快照冷启动噪声 |
| **稳态时长** | **30 分钟** | 足以暴露内存增长、FD 泄漏、分区写入劣化 |
| **峰值吞吐门禁** | **1000 QPS 持续 60 秒**（独立于并发门禁，AC-36） | 直接对应 FR-114 的峰值 1000 QPS；并发门禁压同时在线数、吞吐门禁压每秒请求数，**两者不可互相替代** |
| **流式占比** | **90% 流式 / 10% 非流式** | 主力 Codex CLI 几乎全流式；流式持续占住 goroutine 与连接，是真正压力源 |
| **请求长度** | input ≈ **4000 token**（贴近真实基线 4401），output ≈ **200 token** | 取自 [07 §3bis](./07-axonhub-runtime-probes.md) 真实响应 |
| **上游** | **MOCK**（`verify/mock_upstream.py` 扩展为可控延迟版，可配 TTFT 与产出速率） | 消除中转站 401/520 波动（[15 T4](./15-scope-and-preflight.md)）；**上游不是被测对象** |
| **到达分布** | 泊松到达（非匀速） | 匀速会掩盖排队与抖动 |

### 压力上限探测（**不设通过门槛，但必须做并记录**）

最低标准之外，还须**找到程序自身的天花板**——这是"本身抗压能力越强越好"的落地：

| 阶梯 | 并发用户 | 记录内容 |
| --- | --- | --- |
| 1 | 1000（最低标准） | 必须通过下方判定口径 |
| 2 | 2000 | 记录 P99、错误率、内存、goroutine/连接数 |
| 3 | 4000 | 同上 |
| 4 | 持续加压至**首次不达标** | **记录拐点值**：这就是当前实现的容量天花板 |

→ 拐点值写入 M4 验收证据；后续优化以"抬高拐点"为目标。

### 判定口径（**关键**）

- **P99 ≤50ms 测的是"决策/网关自身开销"，不是端到端延迟**：
  `开销 = 总延迟 − 上游耗时`（[02 `attempts.full_latency_ms − upstream_latency_ms`](./02-data-model.md)、[06 §6](./06-deployment-and-operations.md)）。
  → 直接对应 FR-110 原文"**决策过程给每个请求增加的时延** P99 ≤50 毫秒"。用端到端会把 mock 上游的固有延迟算进来，测不出程序真实开销。
- ⚠️ **`总延迟 − 上游耗时` 观测不到同步写的延迟**（第 11 轮 [high]）：[01 §5.1](./01-architecture.md) 冻结的**首字同步写发生在上游流仍存活期间**，其耗时落在 `upstream_latency_ms` 里，被减法整个抵消；1000 QPS 下光首字+终帧就是 **≈2000 次同步事务/秒**，连接池排队会直接推高用户可感的首字延迟，而现有门禁**照样能通过**。故必须**另立两个专用指标**：

  | 指标 | 定义 | 门禁 |
  | --- | --- | --- |
  | `downstream_ttft_delay_ms` | `attempts.downstream_first_byte_written_at − upstream_first_actionable_at` | P99 阈值**压测中冻结**（见下） |
  | `downstream_finish_delay_ms` | `attempts.downstream_write_completed_at − upstream_terminal_at` | 同上 |

  两者均为**派生指标**，四个时刻列都已在 [02 `attempts`](./02-data-model.md) 落库，无需新增列。

  这两个差值**只包含我们自己的同步写与调度**，上游耗时被完全排除，是同步写代价的唯一可信度量。

- **压测必须开启真实同步持久化**（不得为了跑分把同步写降级为异步），并同时记录：PG **事务提交延迟 P50/P99**、连接池**等待时长与耗尽次数**、每秒同步事务数。
- ⚠️ **设计中"约 1~2ms"是待验证假设，不是结论**：它没有负载条件、PG 延迟分位或连接池容量依据。**M4 压测的产出之一就是给这两个指标定出真实阈值并写回 [01 §5.1](./01-architecture.md) 与 [03 §3.0](./03-upstream-layer.md)**；若实测显著劣于假设，须启用下方的**组提交**优化后重测。
- **允许的优化：组提交（batch commit）**。正确性只要求"**字节放行前该事件已提交**"，**不要求每请求一个事务**。故实现可把 ≤N 个请求的首字/终帧事件合并进一个事务（linger ≤2ms），把 2000 txn/s 压到百量级。该优化**不改变任何恢复语义**——组内任一事件提交即全部提交。
- **失败阈值**：1000 并发稳态错误率 **<0.01%**（超过即不通过）。
- **资源判定**（比 P99 更能暴露程序缺陷）：
  - 稳态 30 分钟内**内存不持续增长**（斜率趋零，排除泄漏）；
  - 压测结束后 5 分钟，goroutine 数与连接数**回落至基线 ±10%**；
  - 全程无 OOM、无 FD 耗尽、无连接池楔死（[03 §6](./03-upstream-layer.md)）。

### 一期不覆盖

测活并发（AC-18 原文提及）随主动测活移入二期（[15 S1](./15-scope-and-preflight.md)）；本轮只压**并发用户强度**。

---

## 3. 验收执行规则

1. **逐里程碑闭环**：该里程碑的 AC **全部通过**才算完成；未通过项必须记录原因与处置（修复 / 降级 / 延期），不得默认通过。
2. **MOCK 类进 CI**：M1/M2 的 MOCK 判定应做成自动化测试，每次提交跑；REAL/LOAD 类在里程碑验收时手动执行并留存证据。
3. **证据留存**：每条 AC 通过时记录证据（命令输出 / 账本查询结果 / diff 结果），存 `docs/acceptance/M{n}-evidence.md`。
4. **不可判定即不通过**：出现"基本正常""大致达标"等表述视为未通过，须给出具体数值或 diff。

---

## 4. 开放点

| # | 开放点 | 结论 |
| --- | --- | --- |
| 1 | REAL 类 AC 依赖中转站稳定性（已多次遇 401/520 波动） | ✅ **已定**：REAL 验收允许重试窗口（如 30 分钟内跑通即算通过）；若上游持续不可用则记录阻塞、不阻断里程碑其余项 |
| 2 | AC-18 压测的具体负载模型 | ✅ **已冻结（2026-07-25）**：见 §2bis。不再留待 M4——否则可用低压力模型"通过"P99，PM 无法复现判定 |
| 3 | 证据留存是否入库 | ✅ **已定**：入库 `docs/acceptance/`，便于回溯"当初怎么验的" |

---

_本篇是 PM 与开发之间的验收契约。**开发交付时对照本表自检，PM 据本表判定**——避免"做完了"与"没达标"的扯皮。_
