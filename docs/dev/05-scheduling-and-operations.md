# 05 调度与经营策略（selector 排序 + steward 经营闭环 · v0.1 草案）

| 项目 | 内容 |
| --- | --- |
| 状态 | ✅ **v1.0 基线（2026-07-26 冻结）** —— 经 28 轮对抗性审查 + 2 轮开发视角走查 + PM 开工前裁决；变更须走版本记录 |
| 日期 | 2026-07-23 |
| 定位 | 把 `selector`（候选选择与排序）与 `steward`（测活/冷却/订阅倾斜/告警/错误预算）落成可实现规则。数值全部来自 [DECISIONS](../DECISIONS.md) 参数确认，**作默认值 + 可配置**（存 [02 §2 `config_params`](./02-data-model.md)，关键项二次确认，FR-115） |
| 输入 | [PRD v1.4](../PRD.md)、[DECISIONS](../DECISIONS.md)（16 参数）、[02 数据模型](./02-data-model.md)、[03 上游对接层](./03-upstream-layer.md)、[01 架构](./01-architecture.md)（selector/steward 模块） |
| 不含 | 流式执行/首字判定/取消传播（属 `executor`，见 03/01）；采集（见 04） |

> 一条总原则贯穿全篇：**决策只读内存快照、P99≤50ms（FR-110）**。所有排序键、预算、门槛均来自后台刷新的快照（价格/健康/余额/订阅），同步路径不查库、不发网络请求。

---

## 1. selector：候选选择与排序

### 1.1 候选过滤顺序（权限/资格先于负载均衡）

对应 ISSUE-001 假设 5 的源码级顺序（过滤先于 LB），逐层裁剪候选集，**顺序不可交换**：

| 序 | 过滤层 | 规则 | 依据 |
| --- | --- | --- | --- |
| 1 | 别名 → 策略 | 由 `model_aliases.policy_id` 定 SLA 等级与测活资格；别名即策略载体 | FR-062/117、AC-25 |
| 2 | 模型能力**与协议** | 按**请求协议**查 `channel_models(channel_id, model_id, protocol)`：只留 `enabled AND support='supported'` 且满足流式/工具需求的 binding。⚠️ 实测存在**非对称支持**（同模型 Responses 通、CC 返 503），不可假设两协议都可用 | FR-005/006、[02 §1.1](./02-data-model.md) |
| 3 | 数据许可 | 按 [02 §2ter `data_policies`](./02-data-model.md) 四维求值（deny 一票否决 → 顺序无关）。**开关 `data_policy_enabled` 默认 `false` → 一期放行全部**；排除原因入 `decision_snapshot` | FR-093（P1，默认关）、AC-14 |
| 4 | 人工停用 → 健康/样本 | **先排除人工停用**：`bindings.enabled=false`、其账号 `status='disabled'`、其故障域 `disabled_until > now()`（三处任一命中即排除，[02](./02-data-model.md)）；再排除 `health_state∈{cooling,disabled}`；`low_confidence` 不作主渠道候选（可作保底）；**`canary` 态仅在 §2.0 五条准入全满足时才作为本次主渠道**（这是它获得样本的唯一途径） | FR-043/046、参数11 |
| 5 | 余额/配额 | 见下方 §1.1bis 的四条判据（**以保守下限而非标称余额判定**） | FR-020~027、**FR-026**、**FR-118** |
| 6 | 价格新鲜度 | 价格 `queried_at` 超 48h 的 binding 退出"低价优选"，仅作保底（参数10 方向：越旧越保守） | FR-014/015、参数10 |
| 7 | 容量保留 | 扣除接管保留与**承诺保留**（`is_committed` 策略专用）后仍有余量；**未登记容量的渠道不设保留、本层直接放行**（§4.3） | 参数12 |

过滤后若候选为空 → 按 §3 全资源不可用处置。

### 1.1bis 余额/配额过滤的四条判据（FR-026 落地）

> ⚠️ **此前只写了"排除 exhausted/critical + 配额 unknown"**，而 [FR-026](../PRD.md) 还要求：*"使用最近可信余额扣除已知消耗形成保守下限；**无法形成安全下限时停止新付费请求**"*。`balance_signals.conservative_floor` 这一列早就建好了，**但 selector 从头到尾没有用过它**——FR-026 的后半句在整套设计里没有任何决策规则承载（本轮路径走查发现）。

| # | 判据 | 处置 |
| --- | --- | --- |
| 1 | `balance_state ∈ {exhausted, critical}` | 排除 |
| 2 | 配额 `unknown` | 排除（FR-118，保守默认） |
| 3 | **`conservative_floor` 可形成且 > 0** | 通过；**用 floor 而非 `last_confirmed_balance` 参与后续判定**（标称余额可能早已被消耗掉） |
| 4 | **`conservative_floor` 无法形成，或 ≤ 0** | **视同 `exhausted` 排除，停止向该资源发新付费请求**（FR-026 后半句） |

**"无法形成"的判定**（`balance_signals`，[02 §7](./02-data-model.md)）：`last_confirmed_balance IS NULL`，**或** `known_consumption_since IS NULL`（消耗不可知则下限无从算起），**或**该快照 `collector_snapshots_v.is_stale = true` 且期间发生过计费（陈旧确认点 + 未知消耗 = 下限不可信）。

- **`conservative_floor ≤ 0` 为何直接排除**：该列**可为负**（[02](./02-data-model.md)），负值意味着"按已知消耗推算，余额可能已经用尽"——继续发付费请求就是在赌，与 FR-026 的字面要求相反。
- **排除原因入快照**：`decision_snapshot.excluded[]` 记 `balance_floor_unavailable` / `balance_floor_exhausted`，与 `data_policy_denied` 同格式，供排障区分"真的没钱"与"算不出有没有钱"。
- **不影响免费/订阅资源**：FR-026 限定"新**付费**请求"；无计费的 binding 不适用判据 4。⏭ 订阅资源一期不在候选（[15 §1.2](./15-scope-and-preflight.md)）。
- **验收**：[AC-29](./14-acceptance-matrix.md) 须补——构造 `last_confirmed_balance` 存在但 `known_consumption_since` 为空的 binding，断言它**被排除**且排除原因为 `balance_floor_unavailable`；再构造 floor 为负的 binding，断言按 `balance_floor_exhausted` 排除。

### 1.2 排序键（冲突优先序可配置）

候选集内排序，**冲突优先序由 policy 决定**（参数2 可配置，默认模板）：

| SLA 等级 | 默认优先序（参数2） | 排序落地 |
| --- | --- | --- |
| 金 | SLA > 缓存 > 成本 | 先按健康达标度(P95 TTFT/成功率) → 缓存作用域可延续 → 实际成功成本 |
| 银 | SLA > 成本 > 缓存 | 健康达标 → 成本 → 缓存 |
| 铜 | 成本 > 缓存 > SLA | 成本 → 缓存 → 健康 |

- **一期成本排序**：按 `price_versions` 的价格版本 × 分组/Key 倍率算预计实际成功成本。⏭ 订阅渠道的**用满倍率**比价（参数14/AC-24）**移入二期**（[15 §1.2](./15-scope-and-preflight.md)）。
- **缓存作用域**：连续会话优先复用 `cache_scopes` 可延续的 binding，切换有损时计入切换成本（FR-055）。
- **SLA 达标度**：用 `resource_health` 的 P95/P99 与目标 `sla_targets.target_value` 比，不用标称值/总体平均（FR-042）。

### 1.3 RoutePlan：候选序列 + 每跳期限

`selector` 输出 `RoutePlan` 交 `executor`。**结构必须冻结**（第 28 轮 [P0]：原文只写 `{binding, deadline_ms}`，而 dispatch 事务需要价格版本、倍率版本、role、预留额、claim 意图——开发只能自己发明这些字段从哪来）：

```go
type RoutePlan struct {
    Entries       []RoutePlanEntry
    EstimatedUSD  float64   // Σ 各跳上界（[02 §2bis](./02-data-model.md) 预估算法）；dispatch 的 :est
    DataClassCtx  Attrs     // 四维数据许可属性，取自 gateway_clients（§1.1 序 3）
}

type RoutePlanEntry struct {
    BindingID           int64
    PriceVersionID      string    // 决策时的基础价版本 → attempts.price_version_id
    MultiplierVersionID string    // 决策时的倍率版本   → attempts.multiplier_version_id
    Role                string    // 'primary' | 'takeover' | 'retry' | 'canary' | 'probe'
    DeadlineMs          int       // 本跳期限
    SingleHopUSD        float64   // 本跳上界 → **必须落 `attempts.single_hop_est_usd`**（[02](./02-data-model.md)）：
                                  // 恢复任务在崩溃后读不到内存里的 RoutePlan
    ClaimIntent         string    // ''（无）| 'canary' | 'probe' —— 决定 dispatch 走哪个变体
    CachePredicted      float64   // FR-056 预测命中率，入 decision_snapshot
    CacheTarget         float64   // 对应目标值，入 decision_snapshot
}
```

**各字段的生成者**：`selector` 填全部——价格/倍率版本取自决策时的内存快照（这正是 FR-013「每请求关联决策时价格版本」的落点）；`executor` **只读不改**；`ledger` 从中取 dispatch 事务的参数。

要点：

- **每跳期限的确定性算法**（第 29 轮 [P0]：原文只给了首跳一句 `min(P95, 目标)`，没说多跳怎么分、会话余量怎么参与、最多几跳 —— 开发只能自己发明，而这直接决定 AC-06/AC-32）：

  ```text
  输入：target      = sla_targets.ttft_p95_ms（承诺策略 5000；无承诺策略取 config 的 default_ttft_budget_ms，默认 8000）
        session_used = 该会话已累计的有效首字（session_prefix_ledger.cumulative_first_token_ms）
        session_target = prefix_target_ms × turn_no    （前缀平均目标，默认 10000/轮）
        margin      = ttft_safety_margin_ms（config，默认 500）

  ① 本轮可用预算 budget = min(target, max(session_target − session_used, min_hop_ms))
       —— 会话已超支时收紧本轮，但不低于 min_hop_ms（默认 1500，太短会导致无谓接管）
  ② 跳数上限 maxHops = min(候选数, max_hops)     （max_hops 默认 3）
  ③ 逐跳分配：hop_i 期限 = min( binding_i 近期 p95_ttft_ms × 1.2 , 剩余预算 − Σ 后续跳的 min_hop_ms − margin )
       —— 「×1.2」给该渠道自身的正常波动留余量；减去后续跳的最小时间，保证接管链跑得完
  ④ 若 hop_i 期限 < min_hop_ms → **不再排入更多跳**（RoutePlan 到此为止）
  ⑤ 最后一跳不设接管期限（无处可切），只受整体超时约束
  ```

  - **无 `p95_ttft_ms` 的 binding**（新渠道/样本不足）：用该 model 全局 p95 的中位数代入，仍无则取 `budget`。
  - **候选耗尽**时按 §3.2 全资源不可用处置（按承诺与否决定排队时长）。
  - **验收**：[AC-06](./14-acceptance-matrix.md) 须断言长会话第 N 轮的本轮预算随 `session_used` 收紧；[AC-32](./14-acceptance-matrix.md) 断言期限到达即 `Close()`。
- 期限到达且未见**内容感知有效首字** → executor `Close()` 传播取消、切下一跳（[03 §4.3](./03-upstream-layer.md)、AC-32）。
- 决策快照(候选/排除原因/选择)写 `requests.decision_snapshot`（元数据 JSON，不含正文，FR-097/112）。

---

### 1.4 接管准入与切换抑制（风险预测 / 防抖动）

候选排序之外，另一组规则约束"何时切换、何时保守"，防止过度接管与频繁抖动：

| 规则 | 内容 | FR |
| --- | --- | --- |
| 风险预测 | 以近期高分位延迟（P95/P99）+ 安全余量预测本轮风险；余额不足时优先选快速稳定资源 | FR-052 |
| 保守准入 | 首轮、短会话、高优先级请求对慢渠道采用更保守的准入条件（更高门槛才让慢渠道进候选） | FR-053 |
| 缓存切换抑制（**一期**，2026-07-26 拉入） | 预测切换后的缓存影响；预计缓存命中低于目标则限制切换——除非更高等级 SLA 要求接管。**触发原因**：主力场景是 Codex 长会话，prompt 前缀长、缓存命中对成本影响显著，不能只做会话粘性 | FR-056、**AC-07** |
| 流量抖动抑制 | 限制单次流量变化幅度，防价格/健康波动造成渠道频繁切换（滞回 + 变更速率上限） | FR-059 |
| 最大等待计算 | 全资源不可用的最大等待（§3.2）由目标首字 + 会话已有余量 + 安全余量 + 接管资源预计延迟共同算出，非固定常数 | FR-077 |

## 2. steward：渠道验证双轨（canary + 主动测活，**均属一期**）

> **2026-07-26 范围调整**：主动测活由二期**拉入一期**（[PRD §2.1](../PRD.md)）。触发原因——运营时段并非全天有量，而 canary **不额外发请求**，夜间等无流量时段新渠道拿不到样本，验证会被拖到几天后。主动测活正是补这个洞。
>
> **两轨并存，分工明确**：
>
> | | §2.0 受控 canary（FR-121） | §2.1~2.3 主动测活（FR-060~067、070~075） |
> | --- | --- | --- |
> | 请求来源 | **本来就要发的**真实业务请求，只改路由目标 | **为探测而额外发起**的合格真实业务请求 |
> | 额外费用 | 无 | 有 → 所以才需要下面的预算体系 |
> | 适用时段 | 有业务流量时（优先，零成本） | **无流量时段的兜底**（如夜间） |
> | 限额机制 | 3 个固定硬上限 | 五维预算 + 资格 + 机会分配 + 错误预算扣减 |
>
> **优先级**：能靠 canary 拿到样本就**不发**测活请求——selector 每次只在 canary 名额已用尽或近 N 分钟无业务流量时，才允许 steward 发起主动测活。

### 2.0 第一轨：受控验证 canary（FR-121，**零额外成本，优先用**）

> ⚠️ **上一版在这里留了个死循环**（对抗性审查第 7 轮 [high]）：一期取消主动测活后，新渠道只能靠真实流量积累样本；但 §1.1 序 4 又规定 `low_confidence` 不作主渠道——**手工启用只改资格、不产生流量**，于是新渠道永远拿不到样本，只可能在主渠道故障时作为未经验证的保底**首次上生产**。等于把渠道验证推迟到故障现场。故一期必须有一个受控验证载体。

| 维度 | canary 轨做法 | 备注 |
| --- | --- | --- |
| 健康数据来源 | **被动累积 + canary 放量**（本节），**canary 本身不额外发请求**；额外探测由 §2.1~2.3 主动测活承担 | §3.1 样本门槛照常适用 |
| 新增 / 冷却期满渠道 | 进入 `canary` 态，按**硬上限**承接真实业务流量 → 达样本门槛转 `observing` → `available` | [02 `resource_health`](./02-data-model.md) |
| 配额/余额 unknown | 默认排除出候选（FR-118），靠 collector 采集刷新，**不靠放量消除** | [04](./04-collector-adapter.md) |
| 记账模式 | canary 用的是本来就要发的请求，无"额外费用"概念 → 不扣测活预算；**FR-073/074/075 的补偿语义由 §2.2 主动测活触发**（一期已启用） | §2.2 |

（两轨对照见本节开头的表；此处不重复。）

**canary 准入（全满足才分配，任一不满足即跳过）**：

| # | 条件 | 默认值（`config_params` 可配） |
| --- | --- | --- |
| 1 | 该请求别名的策略 **`canary_eligible=true`** | **读显式字段，不读等级名**；`is_committed=true` 的策略被 CHECK 约束焊死为不可 canary（[02](./02-data-model.md)） |
| 2 | 该 binding 本窗口已用 canary 数 < 上限 | `canary_max_per_hour = 20` |
| 3 | 该 binding canary 并发 < 上限 | `canary_max_concurrent = 1` |
| 4 | **存在健康的接管候选**（canary 失败能被接管补偿） | 无接管候选即不分配 |
| 5 | 该请求非连续会话的中途轮次（避免破坏缓存前缀） | — |

**跨实例原子占用（claim）—— 硬上限必须落库执行**：

> ⚠️ 第 8 轮 [high]：上一版只在 `resource_health` 放了计数列，而 selector 读的是**内存快照**——双 core 会同时看到 `canary_inflight=0` 各放行一个，默认并发 1 立即被突破；小时上限同理在竞态下失效。"硬上限"必须由数据库的**条件 UPDATE + RETURNING** 执行，内存快照只能做**预筛**。

```sql
-- 单条语句同时完成：窗口翻转、次数递增、并发递增，并原子判定上限
UPDATE resource_health
   SET canary_window_start = CASE WHEN canary_window_start IS NULL
                                   OR canary_window_start < date_trunc('hour', now())
                                  THEN date_trunc('hour', now()) ELSE canary_window_start END,
       canary_used_in_window = CASE WHEN canary_window_start IS NULL
                                     OR canary_window_start < date_trunc('hour', now())
                                    THEN 1 ELSE canary_used_in_window + 1 END,
       canary_inflight = canary_inflight + 1
 WHERE binding_id = :bid
   AND health_state = 'canary'
   AND canary_inflight < :max_concurrent
   AND (canary_window_start IS NULL
        OR canary_window_start < date_trunc('hour', now())      -- 新窗口 → 计数归 1，必过
        OR canary_used_in_window < :max_per_hour)
RETURNING canary_used_in_window, canary_inflight;
```

- **返回 0 行 = 未抢到额度** → **不走 canary**，按正常排序选 binding。抢不到是常态，不是错误。
  > 落到事务层：dispatch 链会分别返回 `quota_ok` / `capacity_ok` / `exp_ok` / `advanced` **四值**（[02 §2bis](./02-data-model.md)）。**`exp_ok=0` 必须 ROLLBACK 后按非 canary 候选重新 dispatch，不得当成 429** —— 二者混判会在 canary 并发竞争时拒绝本可服务的请求。
- **claim 必须与 attempt 同事务，并有可持久化的所有者**（第 9 轮 [high]）：

> ⚠️ 上一版只递增匿名计数、且与 attempt 插入分属两个事务 → ⓐ 崩溃在「claim 成功、attempt 未落库」之间会**永久泄漏 inflight**；ⓑ 兜底规则写成「该 binding 无存活 canary attempt 则归零」，会与**尚未插入 attempt 的正常 claimant** 竞态——提前归零后第二个请求即可进入，`canary_max_concurrent=1` 当场被突破。

表结构见 [02 §6 `canary_claims`](./02-data-model.md)（DDL 唯一真相源在 02）。


| 时点 | 动作 |
| --- | --- |
| **占用** | **单事务内**：① 上面那条条件 UPDATE（0 行即回滚、放弃 canary）② `INSERT canary_claims(state='active')` ③ `INSERT attempts`（意图先行，[01 §5.1](./01-architecture.md)）。三者同生共死——**崩溃在事务内 = 什么都没发生**，不泄漏 |
| **释放** | 在 `finalize` 事务（[02 §2bis](./02-data-model.md)）内：`UPDATE canary_claims SET state='released', released_at=now() WHERE claim_id=:claim AND state='active'`；**仅当影响行数=1** 才 `canary_inflight -= 1`。一次性跃迁 → 重放/并发都只减一次 |
| **回收** | 后台任务（与租约扫描同一个）只回收 `state='active' AND lease_expires_at < now()` 的 claim，按同样的「跃迁成功才减计数」规则。**禁止**按「当前无 attempt 就归零」——那会误伤仍在事务中的 claimant |

- **对 P99≤50ms 的影响**：这条 UPDATE **只在"内存快照判定该 binding 处于 canary 且本次可能分配"时才执行**，正常流量路径**一次都不会跑**（canary 上限默认 20/小时/binding）。若该语句超时（>10ms），立即放弃 canary 走正常路径——**决策路径永不因 canary 阻塞**。

**状态跃迁**：

```
（新登记 / 冷却期满）→ canary
   ├─ 达 §3.1 样本门槛（1h≥20 或 24h≥100）→ observing → （§3.1 观察期规则）→ available
   └─ 连续失败 ≥ canary_failure_threshold(默认 3) → cooling（退避照 §3.1 翻倍），冷却期满重回 canary
```

- canary attempt 落 `attempts.role='canary'`，**正常计费、正常计入成本**；若该跳被 SLA 接管取消，其 claim 由 [`closeout_attempt`](./02-data-model.md) **在该跳结束时立即释放**，不等租约过期；其失败**计入该 binding 健康统计**，但因别名限定为铜级，**不消耗金/银错误预算**。
- 运维可 `POST /admin/bindings/{id}/canary` 手工把某 binding 置回 canary 并重置窗口计数（[09](./09-admin-api.md)）。

> **canary 的固有局限**：长期零业务流量时（例如夜间无请求）它拿不到样本——因为它不额外造流量。此时渠道停在 `canary` 且 `low_confidence=true`，行为是安全的（保守），只是验证被推迟。**这正是 §2.1~2.3 主动测活要补的洞**（2026-07-26 拉入一期）：近 N 分钟无业务流量时，由 steward 在预算内主动发起探测请求。
>
> **验收**：[AC-08](./14-acceptance-matrix.md) 须断言新建 binding 与冷却期满 binding 都能**在不发生全局故障的前提下**走完 `canary → observing → available`。

### 2.1 预算三层封顶（参数3，均可配置）

| 层 | 上限 | 落地 |
| --- | --- | --- |
| 全局 | 测活费用 ≤ 月度总请求费用 **2%**，且日封顶 = 月预算 ÷ 20 | `attempt_usage JOIN attempts JOIN requests` 后按 **`requests.probe_kind='probe'`** 过滤滚动累计（⚠️ `attempt_usage` **没有** `probe_kind` 列；attempt 层的对应字段是 `attempts.role='probe'`） |
| 租户 | 单租户测活 ≤ 该租户月费用 **2%**；同租户同时 ≤ **1** 个测活请求 | 费用走 `probe_budget_windows`；**并发闸走 `probe_claims` 的 active 计数**（见 §2.1bis 补充语句）—— 第 31 轮修正：此前只说"并发闸"而预算 SQL 只扣费用/次数，没有任何并发判定载体 |
| 会话 | 每会话最多被测活 **1** 次；会话第一轮、金级会话默认不测活 | `session_prefix_ledger` / 会话级标记 |
| 错误预算 | 测活导致的失败 ≤ 该等级错误预算 **10%** | §5 错误预算扣减，超限停测活。⚠️ **仅 canary 轨扣用户错误预算**（它用的是真实用户请求）；主动测活轨的失败**不进用户 SLA 分母**，只扣 `probe_budget_windows.failure_count`（[PRD AC-09](../PRD.md)） |

> **五维限制（FR-063）**：上表列全局/租户/会话/错误预算；此外**用户级**与**资源级**同样分别设测活费用/次数/并发/错误预算上限（`config_params` 按 scope 分别配置）。

### 2.1bis 主动测活的触发、抢占与模板选择（第 29 轮 [P0]）

> 此前只有预算**上限表**，没有"谁定时触发、谁抢执行权、预算怎么原子扣、模板怎么选" —— 开发无从下手。
> **里程碑归属冻结：M4**（与 canary 闭环 AC-08、冷却/样本门槛同批交付，都属 steward 经营闭环）。M2 只做流式 SLA 核心，不含任何测活。

**触发条件**（每 60s 一轮，advisory lock #3 抢占，见 [§5bis](#5bis-后台任务总表)）：

```text
候选 binding = health_state = 'canary'
             AND 该 binding 近 probe_idle_window（默认 30min）内**无任何真实业务 attempt**
             AND low_confidence = true            -- 还没攒够样本
             AND 存在健康接管候选（失败能补偿）
排序 = 样本陈旧度 desc, 失败历史 asc   （FR-064 的机会分配：不得持续集中同一 binding）
每轮最多取 probe_batch_size（默认 3）个
```

- **canary 优先原则的落地就在这里**：只有"近 30min 无业务流量"才进候选 —— 有流量时 canary 会自然拿到样本，不必额外花钱。

**五维预算的原子扣减**（与 canary claim 同构，缺一维都可能被并发突破）：

```sql
-- probe dispatch：预算 + 并发闸 + claim + attempt，**单事务**
-- ⚠️ 第 31 轮修正：上一版把并发闸的 CTE 写在了 SELECT **之后**，SQL 根本不可执行；
--    且 probe_claims / attempts 的插入没有并进这个事务。整段重写如下。
BEGIN;
-- ⚠️ 先确保当日窗口行存在（第 38 轮：此前只有 UPDATE，行不存在时**恒 0 行**，
--    等于每天第一次探测必然失败，且看不出原因）。五个 scope 一次建齐。
INSERT INTO probe_budget_windows(scope_kind, scope_id, window_start)
VALUES ('global','*',:day), ('tenant',:tenant,:day), ('user',:client,:day),
       ('session',:session,:day), ('binding',:bid,:day)
ON CONFLICT DO NOTHING;

WITH
  -- ① 五维预算：各自条件 UPDATE，任一为 0 行即整链失败
  g AS (UPDATE probe_budget_windows SET probe_count=probe_count+1, probe_cost=probe_cost+:est
         WHERE scope_kind='global'  AND scope_id='*'     AND window_start=:day
           AND probe_cost + :est <= :global_cap                      RETURNING 1),
  t AS (UPDATE probe_budget_windows SET probe_count=probe_count+1, probe_cost=probe_cost+:est
         WHERE scope_kind='tenant'  AND scope_id=:tenant AND window_start=:day
           AND probe_cost + :est <= :tenant_cap                      RETURNING 1),
  u AS (UPDATE probe_budget_windows SET probe_count=probe_count+1, probe_cost=probe_cost+:est
         WHERE scope_kind='user'    AND scope_id=:client AND window_start=:day
           AND probe_cost + :est <= :user_cap                        RETURNING 1),
  b AS (UPDATE probe_budget_windows SET probe_count=probe_count+1, probe_cost=probe_cost+:est
         WHERE scope_kind='binding' AND scope_id=:bid    AND window_start=:day
           AND probe_count + 1 <= :binding_cap                       RETURNING 1),
  -- session 维（FR-063 第五维：每会话最多被测活 1 次）。第 38 轮补：
  -- §2.1 写了这条限制，但此前 SQL 里没有任何 session 维扣减。
  -- 模板探测不属于用户会话时 :session 传固定哨兵 '*'，该维恒放行。
  ss AS (UPDATE probe_budget_windows SET probe_count=probe_count+1
          WHERE scope_kind='session' AND scope_id=:session AND window_start=:day
            AND probe_count + 1 <= :session_cap                        RETURNING 1),
  e AS (SELECT 1 WHERE (SELECT failure_count FROM probe_budget_windows
                         WHERE scope_kind='global' AND scope_id='*' AND window_start=:day)
                       < :error_budget_cap),
  -- ② 并发闸（第六维）：同租户在飞的 probe 数，取自 probe_claims 的 active 行
  c AS (SELECT 1 WHERE (SELECT count(*) FROM probe_claims pc
                          JOIN requests r ON r.id = pc.request_id
                         WHERE pc.state='active' AND pc.lease_expires_at > now()
                           AND r.tenant_id = :tenant)
                       < :tenant_probe_concurrency),
  -- ③ 容量占用（第 36 轮补：探测同样要占上游容量，kind='probe'、天花板 70%）
  cap_res AS (   -- 未登记容量时换成 SELECT :bid AS binding_id FROM g 直通
    UPDATE resource_health h
       SET rpm_window_start = CASE WHEN h.rpm_window_start IS NULL
                                    OR h.rpm_window_start < date_trunc('minute', now())
                                   THEN date_trunc('minute', now()) ELSE h.rpm_window_start END,
           rpm_used = CASE WHEN h.rpm_window_start IS NULL
                            OR h.rpm_window_start < date_trunc('minute', now())
                           THEN 1 ELSE h.rpm_used + 1 END,
           concurrency_inflight = h.concurrency_inflight + 1
      FROM g, t, u, ss, b, e, c, bindings bd
     WHERE h.binding_id = :bid AND bd.id = h.binding_id
       AND (bd.concurrency_limit IS NULL
            OR h.concurrency_inflight < floor(bd.concurrency_limit * :ceiling_probe))
       AND (bd.rpm_limit IS NULL OR h.rpm_window_start IS NULL
            OR h.rpm_window_start < date_trunc('minute', now())
            OR h.rpm_used < floor(bd.rpm_limit * :ceiling_probe))
    RETURNING h.binding_id),
  cap AS (
    INSERT INTO capacity_claims(claim_id, binding_id, request_id, attempt_id, kind,
                                lease_owner, lease_expires_at)
    SELECT :cap_claim_id, binding_id, :rid, :attempt_id, 'probe', :owner,
           now() + interval '60 seconds' FROM cap_res
    RETURNING request_id),
  -- ④ 七者全过才插 probe claim
  claim AS (
    INSERT INTO probe_claims(claim_id, binding_id, template_id, request_id, attempt_id,
                             lease_owner, lease_expires_at)
    SELECT :claim_id, :bid, :template_id, :rid, :attempt_id, :owner, now() + interval '60 seconds'
      FROM cap                                   -- 任一前置 CTE 为空 → 链断 → 不插
    RETURNING request_id),
  -- ⑤ attempt 与 claim 同事务（意图先行）
  att AS (
    INSERT INTO attempts(id, request_id, request_created_at, attempt_no, binding_id,
                         single_hop_est_usd, price_version_id, multiplier_version_id, role,
                         attempt_status, lease_owner, lease_heartbeat_at)
    SELECT :attempt_id, :rid, :rcat, 1, :bid, :est, :pv, :mv, 'probe', 'pending', :owner, now()
      FROM claim
    RETURNING id)
SELECT (SELECT count(*) FROM g)   AS g_ok,  (SELECT count(*) FROM t) AS t_ok,
       (SELECT count(*) FROM u)   AS u_ok,  (SELECT count(*) FROM b) AS b_ok,
       (SELECT count(*) FROM ss)  AS ss_ok, (SELECT count(*) FROM e) AS e_ok,
       (SELECT count(*) FROM c)   AS c_ok,
       (SELECT count(*) FROM cap) AS capacity_ok,
       (SELECT count(*) FROM att) AS dispatched;
-- ⚠️ **块内不写 COMMIT**（第 33 轮修正：原版紧跟 COMMIT，而下一行又要求
--    任一值为 0 时 ROLLBACK —— 先提交就回滚不了，预算已扣、claim 没插）。
--    提交与否**一律由应用层依返回值决定**，与 dispatch 同纪律。
-- 应用层：`dispatched=1` 才 COMMIT 并发起探测；**任何一值为 0 一律 ROLLBACK**
--   ss_ok=0 → 该会话已被测活过（每会话上限 1 次）
--   capacity_ok=0 → 该 binding 上游容量已满，本轮跳过它（不是错误）
--   （否则预算已扣、claim 却没插 —— 额度白白漏掉）
--   g/t/u/ss/b=0 → 该维预算耗尽；e=0 → 错误预算超限，暂停全部测活；c=0 → 该租户已有在飞探测
```

> ⚠️ 各行**不存在**时视为未初始化 → 由本 worker 先 `INSERT ... ON CONFLICT DO NOTHING` 建当日行，再执行上面的 UPDATE。
> ⚠️ **`session` 维度一期不参与**：探测不属于任何用户会话（[FR-063](../PRD.md) 已注明一期实际只有 global/binding/client 三个独立维度，此处的 tenant/user 在个人场景同为 client id）。

**模板选择算法**（`probe_templates`，[02 §6ter](./02-data-model.md)）：

```text
1. 按 (binding.model_id, 请求协议) 过滤 enabled=true 的模板
2. 优先选**该 binding 最久未用过**的模板（`probe_claims` 里该 binding+template 的最近 created_at）
   —— 轮换模板，避免只测一种负载形态
3. 若该模型无任何模板 → **跳过该 binding 并记 P3 告警**（`category='probe_template_missing'`）
   ⚠️ 不得回退到 hello/ping：FR-060 明令禁止，宁可不测也不要测出一个虚假的"健康"
4. 选中后按 canary 同构方式 claim（probe_claims），claim + attempt 同事务
```

**探测请求的记账主体**：内置凭证 `system-probe`（[02 §2bis](./02-data-model.md)）；`requests.probe_kind='probe'`、`attempts.role='probe'`。

### 2.2 测活资格（别名承载，参数6）

- **测活权限编进对外别名**：`gpt-5.5`（可测活）vs `gpt-5.5-sla-1`（禁测活）→ `routing_policies.probe_allowed`（AC-25）。
- 记账模式（参数4、**FR-073/074**）：默认全部业务**严格补偿**——测活拖慢会话后**优先用快速渠道恢复会话指标**（FR-074）；`预算延续`（不强制补偿，有损体验）仅限内部批处理/离线评估/开发测试三类白名单，每次启用登记业务名/时段/责任人（FR-075）。

---

### 2.3 测活前置条件、机会分配与记录

- **前置条件（FR-061，全满足才可测活）**：模型能力匹配 + 数据许可 + 价格有效（新鲜）+ 余额充足 + 容量充足（不占接管保留）+ 会话可承受（非金级首轮）+ 接管资源可用（失败能补偿）。任一不满足即不测活。
- **机会分配（FR-064）**：试错机会按 ①样本陈旧程度 ②指标不确定性 ③潜在收益 ④失败历史 加权分配，**不得持续集中给同一用户或渠道**（分配器维护 per-user / per-binding 近期测活计数，超集中度阈值即降权）。
- **记录（FR-067）**：每次测活记 测活原因、决策快照、预算变化、首字、完整结果、缓存、费用、接管结果 → 落 [02](./02-data-model.md) `requests.decision_snapshot` + `requests.probe_kind='probe'` + `attempts.role='probe'`。
- **达上限暂停（FR-068）**：探索预算达任一层上限即暂停新测活，**不影响正常稳定流量**（测活闸与主流量闸独立）。

## 3. steward：冷却、样本门槛与全资源不可用

### 3.1 样本门槛与观察期（参数11，可配置）

| 项 | 默认 | 落地（[02 `resource_health`](./02-data-model.md)） |
| --- | --- | --- |
| 可作主渠道 | 近 1h ≥20 次 或 近 24h ≥100 次真实请求 | `sample_count_1h/24h`；不足则 `low_confidence=true` |
| 观察期恢复 | 故障恢复后 30min 或连续 50 次成功（先到为准）才转"可用" | `observing_since`/`observing_success_count` |
| 冷却退避 | 5min 起步，连续失败每次翻倍，最长 4h | `cooldown_current_sec`(300→×2→≤14400)、`consecutive_failures` |

### 3.2 全资源不可用处置（参数7，时长可配置）

| 等级 | 排队等待 | 无渠道时 |
| --- | --- | --- |
| 金 | 最多 15s（等渠道恢复） | 返回明确"服务暂不可用" + 建议重试时间 + 事件编号 |
| 银 | 最多 5s | 同上 |
| 铜 | 0s 直接返回 | 同上 |

任何等级**不偷换低配模型、不使用不合规渠道**（FR-096 硬性）。

---

## 4. steward：订阅倾斜与容量保留

### 4.1 订阅倾斜三道闸（参数15，全满足才倾斜）

> ⏭ **本节已移入二期**（[15 §1.2](./15-scope-and-preflight.md)）：一期不实现订阅倾斜；排序键中的**用满倍率**同步推迟，一期成本排序仅用普通渠道价格版本×倍率。

到期作废前可适当多用订阅渠道，但三闸缺一不可（[02 `subscription_waste_forecast`](./02-data-model.md)）：

| 闸 | 阈值 | 判据 |
| --- | --- | --- |
| 成本上限 | 选订阅渠道后预计成本 ≤ 当前最优普通渠道成本 **1.5 倍** | 用满倍率比价 |
| 引流上限 | 单订阅渠道 ≤ 该模型总流量 **40%** | 滚动流量占比 |
| 启动条件 | 剩余有效期 ≤ **7 天** 且 预计浪费 ≥ 周期额度 **20%** | `remaining_days` + `predicted_wasted_quota` |

不可安全利用时允许到期并记录原因与损失（FR-039/AC-21）：`unusable_reason`（倍率/性能/稳定性不达标）。

### 4.2 容量保留（参数12，可配置）

| 保留 | 默认 | 用途 |
| --- | --- | --- |
| 承诺保留 | 每渠道 20% 容量（RPM+并发）仅 `is_committed=true` 的策略可用 | 承诺流量不被日常挤占。**读策略字段不读等级名** |
| 测活保留 | 测活 ≤ 全局容量 5%，且不占接管保留 | 探索不伤主流量 |
| 接管保留 | 每模型 ≥ 一个健康快渠道的金级峰值并发 10% 专用接管 | SLA 最后防线（FR-029/032） |

---

### 4.3 容量保留的执行载体（一期降精度不降语义）

> 此前 §4.2 只有三档保留比例，**没有任何执行机制** —— 容量基数从哪来、谁扣减、并发怎么算，全都没定义。

**容量基数来源**：`bindings.rpm_limit` / `concurrency_limit`（[02](./02-data-model.md)），由 `/admin/bindings` 人工登记，或采集器的 `RateLimit()` 自动回填（`capacity_source` 记来源）。

**⚠️ 未登记容量 = 该渠道不启用任何保留**（明示的降级）：

| 情形 | 行为 |
| --- | --- |
| 两列皆空 | 过滤序 7 **直接放行**，不做任何保留判定 |
| 已登记 | 按 §4.2 三档比例扣减，用与 canary 同构的 **DB 原子计数**执行 |

- **为什么允许这种降级**：多数中转站不公开 RPM/并发上限，强行要求登记会让大部分渠道不可用。宁可"没保留"也不要"假装有保留"。
- **必须让运维看见**：`/admin/bindings` 列表与渠道详情**须显式标注"未登记容量 → 保留未生效"**（[09](./09-admin-api.md)）。否则运维会以为保留在工作，而实际金级流量随时可能被日常请求挤掉。
- **只对"启用保留"的渠道加闸**：普通渠道不走这条 DB 原子路径，保住 [FR-110](../PRD.md) 的 P99≤50ms —— 加闸的渠道数量由运维登记量控制，是可预期的。
- **验收前置**：[AC-18](./14-acceptance-matrix.md) 的测试种子**必须登记容量**，否则保留层被放行、该 AC 无从验证。

---

### 4.4 计费异常检测（FR-016，M3）

**判据**（滚动窗口内按 binding 聚合）：

```text
偏差率 = abs(Σ cost_variance) / Σ estimated_cost
超容差 ⟺ 偏差率 > billing_variance_tolerance   （默认 0.05，config_params，**关键项走二次确认**）
```

⚠️ **除零保护**：`Σ estimated_cost` 可能为 0（免费渠道、估算失败、上游返回 0 token）。故：

| 情形 | 判据 |
| --- | --- |
| `Σ estimated_cost >= billing_min_base`（默认 $0.01） | 用上面的**比率**判定 |
| `Σ estimated_cost < billing_min_base` | 改用**绝对差额**：`abs(Σ cost_variance) > billing_abs_tolerance`（默认 $0.05） |

**触发后**：`alert_events(category='billing_anomaly', severity='P2')` + 该 binding **退出低价优选**（仍可作保底），`decision_snapshot.excluded[]` 记 `billing_anomaly`。

**恢复条件**（二选一）：人工确认；或连续 `billing_recovery_streak`（默认 20）次结算一致。

**对账查询**：`GET /admin/ledger/reconciliation`（[09](./09-admin-api.md)）——按 account/key/model 聚合预估 vs 实扣 vs 余额变化，不可归因差额单列 `unattributed`（FR-019）。

---

## 5. steward：错误预算、SLA 统计与告警

### 5.1 SLA 等级与统计（参数1，全可配置）

- 等级数量/各指标数值/统计窗口**全部用户自定义**（参数1：圈的渠道不同，SLA 就不同）；金/银/铜三级仅默认模板（[02 `sla_targets`](./02-data-model.md)、§11）。
- 统计口径：`有效可用性 = 完整成功数 ÷ 应计入 SLA 的有效请求数`；用户参数错误、用户主动取消不计入失败（PRD §术语）。
- **崩溃恢复产生的终态照常计入用户 SLA**：`final_status='failed'`（`unknown_billing` 场景）与 `'interrupted'`（首字后崩溃）都**计入失败**，后者是否计入 `stream_break_rate` 由**唯一判据**决定——`stream_broken=true OR (final_status='interrupted' AND downstream_first_byte_written_at IS NOT NULL)`，分母为同窗口全部流式请求（[02 §4.2bis](./02-data-model.md)）——FR-071 要求用户真实经历的失败与流中断始终计入，AC-12 要求首字后中断记为完整失败。**但这些 attempt 不计入渠道健康统计**（崩溃是我们的故障，不是渠道的），两套口径的完整对照见 [02 §4.2bis 不变式 3](./02-data-model.md)。
- 错误预算 = 窗口内允许失败上限（如 99.9% → 月 ~43min）；测活失败扣该等级预算 ≤10%（§2.1）。

### 5.2 告警分级（参数16，分类与时限可配置）

| 级别 | 典型事件 | 时限 | 落地（[02 `alert_events`](./02-data-model.md)） |
| --- | --- | --- | --- |
| P1 | 余额耗尽、Key 失效、全渠道不可用、安全违规 | 即时、15min 内响应 | **不得延迟**（FR-103）；`severity='P1'` |
| P2 | 错误预算消耗过快、SLA 超标、订阅到期损失风险、同故障域集中失败 | 1h 内 | `severity='P2'` |
| P3 | 数据过期、价格异常、容量趋紧 | 当日 | `severity='P3'` |

同因合并为一个持续事件（`dedup_key`，FR-102），不刷屏。

### 5.2bis 告警写入路径（第 29 轮 [P0]：此前只有分级示例，没有 `dedup_key` 规则与事务）

**`dedup_key` 生成规则**（同因合并的唯一依据，FR-102）：

| category | `dedup_key` | 说明 |
| --- | --- | --- |
| `balance_exhausted` | `balance:<balance_group_key>` | 同一余额组只报一次，不按 binding 刷屏 |
| `key_invalid` | `key:<key_id>` | |
| `all_unavailable` | `unavail:<model_id>` | 按模型聚合——一个模型全挂是一件事 |
| `billing_anomaly` | `billing:<binding_id>` | |
| `collector_failed` | `collector:<channel_id>:<capability>` | 区分是价格挂了还是余额挂了 |
| `unknown_billing` | `unkbill:<request_id>` | **每单一条**，因为需要逐单人工核对 |
| `error_budget_burn` | `budget:<policy_id>:<window_start>` | 同窗口只报一次 |
| `probe_template_missing` | `tmpl:<model_id>:<protocol>` | |
| `capacity_tight` | `capacity:<binding_id>` | |

**写入事务**（`open` 与 `recovering` 的并发转换必须在行锁下做，否则会产生第二条活动行）：

```sql
BEGIN;
-- ⚠️ `id` 无 DEFAULT（UUIDv7 由应用层生成），必须显式给；`severity` 是 TEXT，
--    直接 GREATEST 会按字典序比较（'P1' < 'P2' < 'P3'），**恰好与严重度相反** ——
--    P1 最严重却会被 P3 覆盖。改用内联 CASE（不引入自定义函数——全库没有 CREATE FUNCTION）。
INSERT INTO alert_events (id, dedup_key, category, severity, state,
                          started_at, last_seen_at, occurrence_count, payload)
VALUES (:alert_id /* UUIDv7 */, :key, :cat, :sev, 'open', now(), now(), 1, :payload)
ON CONFLICT (dedup_key) WHERE state <> 'closed'      -- 部分唯一索引 uq_alert_active
DO UPDATE SET last_seen_at = now(),
              occurrence_count = alert_events.occurrence_count + 1,
              -- ⚠️ 内联 CASE，**不依赖任何自定义函数**（第 37 轮：上一版写
              --    severity_rank()，但全库没有 CREATE FUNCTION，照抄跑不了）
              severity = CASE
                WHEN alert_events.severity = 'P1' OR EXCLUDED.severity = 'P1' THEN 'P1'
                WHEN alert_events.severity = 'P2' OR EXCLUDED.severity = 'P2' THEN 'P2'
                ELSE 'P3' END
RETURNING id, state;
-- 语义：**升级不降级**。不可用 GREATEST —— severity 是 TEXT，
-- 'P1'<'P2'<'P3' 的字典序与严重度**恰好相反**，GREATEST 会让 P1 被 P3 覆盖。
COMMIT;
```

**状态迁移**（`open → acknowledged → recovering → closed`）一律 `SELECT … FOR UPDATE` 后再改。**关闭条件**：该 `dedup_key` 的触发条件连续 `alert_recovery_checks`（默认 3）轮不再成立；`unknown_billing` 类**只能人工关闭**。

**P1/P2/P3 的判据**（[AC-19](./14-acceptance-matrix.md) 三场景的精确触发）：

| 场景 | 判据 | 级别 |
| --- | --- | --- |
| 余额耗尽 | `conservative_floor <= 0` **或** `balance_state='exhausted'` | **P1** |
| Key 失效 | 该 key 连续 `key_invalid_streak`（默认 3）次收到 401/403 | **P1** |
| 全渠道不可用 | 某 model 的候选集连续 `all_unavail_checks`（默认 2）轮为空 | **P1** |
| 错误预算快速消耗 | 窗口内已消耗 > `error_budget_burn_ratio`（默认 0.5）且窗口过半未到 | **P2** |
| 计费异常 | §4.4 判据 | **P2** |
| 数据过期 | `collector_snapshots_v.is_stale` 持续 > `stale_alert_hours`（默认 12h） | **P3** |

---

### 5.3 余额不足信号自适应识别（参数5/FR-027）

余额**非实时**、后台校对；置"耗尽"靠多判据组合（[02 `balance_signals`](./02-data-model.md)）：错误码 / 错误文案正则("余额"类关键词) / 真实请求失败信号 / 余量归零（AC-29）。保守储备与三档处置（<24h 告警 / <6h 停测活与高成本 / <1h 临界减普通流量，参数5）。

---

## 5bis. 后台任务总表（第 29 轮 [P0]：此前八个 worker 散落各处，**没有一处说清周期、多实例协调与失败重试** —— 开发无法判断谁定时跑、谁抢占执行权）

**统一的多实例协调机制**：所有周期任务用 **PG advisory lock**（`pg_try_advisory_lock(<任务号>)`）抢执行权，抢不到就跳过本轮 —— 与 [06 §2.2](./06-deployment-and-operations.md) 的 bootstrap 选主同一套机制，不引入额外组件。**逐行扫描类**（恢复扫描、outbox 投递）改用 `FOR UPDATE SKIP LOCKED`，天然多实例安全，不需要 advisory lock。

| # | 任务 | 周期 | 多实例协调 | 失败处置 | 定义位置 |
| --- | --- | --- | --- | --- | --- |
| 1 | **outbox 投递** | 200ms | `SKIP LOCKED` | 指数退避重试，行保留；积压进 `/metrics` | [02 §9.2bis](./02-data-model.md) |
| 2 | **恢复扫描** | 启动时 + 每 30s | `SKIP LOCKED` | 记日志重试；**必须在 outbox drain 之后**跑 | [02 §4.2bis](./02-data-model.md) |
| 3 | **健康聚合** | 每 60s | advisory lock #1 | 跳过本轮，下轮补 | §5bis.1 |
| 4 | **余额下限重算** | 每 5min | advisory lock #2 | 跳过；`conservative_floor` 保持旧值并因陈旧而更保守 | §5bis.2 |
| 5 | **主动测活调度** | 每 60s | advisory lock #3 | 跳过 | §2.1bis |
| 6 | **采集器** | 价格 6h／余额 5min／Key 额度 30min | advisory lock #4~6（按类分） | 该站进退避，其它站不受影响 | [04](./04-collector-adapter.md) |
| 7 | **capacity/canary/probe claim 回收**（**三张表同一任务**） | 每 60s | advisory lock #7 | 跳过 | [02 §6bis](./02-data-model.md)、[§6bis-2](./02-data-model.md) |
| 8 | **分区维护**（建下月分区、清过期） | 每天 03:00 | advisory lock #8 | P2 告警 | [02 §9.1](./02-data-model.md) |

> **快照刷新不在此表**：selector 读的内存快照由各任务写库后**主动推送**给本实例（或按 `config_params` 的 `snapshot_refresh_ms` 拉取），属实例内行为，不需要跨实例协调。

### 5bis.0 `resource_health` 的并发写入分工（第 31 轮自查 [high]）

> 全库有 **9 处**写 `resource_health`，横跨请求路径（claim/release）与三个后台任务（健康聚合、状态迁移、claim 回收）。此前**没有一处说明它们会不会互相覆盖** —— 开发只能自己猜要不要加锁，猜错就是计数错乱或死锁。

**分工原则：按列切分，各改各的,不加行锁。**

| 列 | 唯一写入者 | 写法 |
| --- | --- | --- |
| `concurrency_inflight`、`canary_inflight` | 请求路径的 claim / release，以及 claim 回收任务 | **只做 `+1` / `GREATEST(-n,0)` 增量**，从不整列覆盖 |
| `rpm_used`、`rpm_window_start`、`canary_used_in_window`、`canary_window_start` | 请求路径的 claim | 条件 UPDATE 内翻转窗口 + 递增 |
| `sample_count_*`、`p95_*`、`success_rate`、`low_confidence`、`last_sample_at` | **健康聚合 worker**（每 60s，advisory lock #1） | 整列覆盖（它是唯一写者） |
| `health_state`、`observing_*`、`cooldown_*`、`consecutive_failures`、`canary_failures` | **健康聚合 worker 的状态迁移段**（同一事务） | 整列覆盖 |
| `canary_since` | 状态迁移段 | 覆盖 |

**因此三类写入天然不冲突**：增量列只被增量修改（PostgreSQL 行级写锁保证单条 UPDATE 原子），统计列只有一个写者。**不需要显式行锁,也不会死锁**——不同事务改同一行的不同列时，PG 仍会串行化该行的写，但因为都是短事务且无循环等待，不产生死锁。

⚠️ **唯一需要注意的**：健康聚合的状态迁移会把 `health_state` 从 `canary` 改走，而此刻可能有 in-flight 的 canary claim。**不回收它们** —— 让它们正常跑完并释放（`canary_inflight` 自然归零）；新的 canary 因 `health_state` 已变而不再被分配。**不得**在状态迁移里强行清零 `canary_inflight`，那会让仍在执行的 claim 释放时把计数减成负数（虽有 `GREATEST` 兜底，但会掩盖真实占用）。

### 5bis.1 健康聚合 worker（每 60s）

```sql
-- 从 attempts 聚合到 resource_health。⚠️ 归因口径见 §4.2bis 不变式 3：
--    unknown_billing / interrupted 是**我们**崩溃，不计入渠道成功率。
WITH w AS (
  SELECT a.binding_id,
         count(*) FILTER (WHERE a.started_at > now() - INTERVAL '1 hour')  AS n1h,
         count(*) FILTER (WHERE a.started_at > now() - INTERVAL '24 hours') AS n24h,
         percentile_disc(0.95) WITHIN GROUP (ORDER BY a.content_aware_ttft_ms)
           FILTER (WHERE a.has_ttft_output)                                 AS p95,
         avg((a.attempt_status = 'completed')::int)                         AS ok_rate
    FROM attempts a
   WHERE a.request_created_at > now() - INTERVAL '24 hours'
     AND a.attempt_status NOT IN ('unknown_billing','interrupted')   -- 崩溃不算渠道的账
     AND a.role <> 'probe'                                            -- 测活失败单独归因（§2.1）
  GROUP BY a.binding_id)
UPDATE resource_health h
   SET sample_count_1h = w.n1h, sample_count_24h = w.n24h,
       p95_ttft_ms = w.p95, success_rate = w.ok_rate,
       low_confidence = NOT (w.n1h >= :min_1h OR w.n24h >= :min_24h),
       last_sample_at = now(), updated_at = now()
  FROM w WHERE h.binding_id = w.binding_id;
```

**状态迁移（同一事务内，紧接上面）**，判据全部来自刚更新的行：

| 从 | 到 | 条件 |
| --- | --- | --- |
| `canary` | `observing` | `NOT low_confidence`（达 §3.1 样本门槛）且 `canary_failures < 阈值` |
| `canary` | `cooling` | `canary_failures >= canary_failure_threshold`（默认 3） |
| `observing` | `available` | `observing_since < now()-30min` **或** `observing_success_count >= 50` |
| `available` | `degraded` | `success_rate < 目标` 或 `p95_ttft_ms > 目标`，连续 2 轮 |
| `available`/`degraded` | `cooling` | `consecutive_failures` 触发退避（§3.1） |
| `cooling` | `canary` | `cooldown_until < now()` —— **回 canary 而非直接 available**，重新积累样本 |

### 5bis.2 余额下限重算 worker（每 5min）

```text
conservative_floor(account_group) =
      last_confirmed_balance                       -- 采集器最近一次确认值
    − known_consumption_since                      -- 见下
    − safety_reserve                               -- = max(余额 × `balance_safety_reserve_ratio`(0.02),
                                                   --        `balance_safety_reserve_min_usd`($1)) —— 两键见 [09 §4bis](./09-admin-api.md)

known_consumption_since = Σ attempt_usage.total_cost
                          WHERE attempt.binding 属该账号组
                            AND attempt.started_at > last_confirmed_at
                            AND attempt_status IN ('completed','failed','interrupted','unknown_billing')
                          （**含**已计费但结果不明的两类——保守）
```

- **账号组聚合**：按 `upstream_accounts.balance_group_key` 归并（FR-022/AC-04）——同一 key 的多账号/多 Key **只算一份余额**，消耗则**全部累加**。`balance_group_key` 为空时按 account_id 独立成组。
- **无法形成下限**的三种情形与处置见 [§1.1bis](#11bis-余额配额过滤的四条判据fr-026-落地)，本 worker 只负责在这些情形下**把 `conservative_floor` 置 NULL**（而非算出一个假值）。
- **落 `balance_signals`**：每轮写一行快照（`last_confirmed_balance`/`known_consumption_since`/`conservative_floor`/`computed_at`），供排障回溯"当时为什么排除了这个渠道"。

---

## 6. 参数 → 落点映射（回溯 DECISIONS）

| 参数 | 本篇落点 | 表 |
| --- | --- | --- |
| 1 SLA 指标可配置 | §5.1 | `sla_targets`/`config_params` |
| 2 冲突优先序 | §1.2 | `routing_policies.conflict_order` |
| 3 测活预算 | §2.1 | `config_params`(probe.*) + `attempt_usage` |
| 4 测活记账模式 | §2.2 | `config_params` + 白名单 |
| 5 余额安全垫/信号识别 | §5.3 | `balance_signals` |
| 6 禁测活/别名控制 | §2.2 | `model_aliases`/`routing_policies.probe_allowed` |
| 7 全资源不可用 | §3.2 | `routing_policies.no_resource_wait_ms` |
| 10 数据时效 | §1.1 序6 | `config_params`(freshness.*) |
| 11 样本/冷却 | §3.1 | `resource_health` |
| 12 容量保留 | §4.2 | `config_params`(capacity.*) |
| 13/14 订阅台账/双倍率 ⏭二期 | §1.2、§4.1 | `subscription_plans.usable/actual_multiplier` |
| 15 订阅倾斜三道闸 ⏭二期 | §4.1 | `subscription_waste_forecast` |
| 16 告警分级 | §5.2 | `alert_events` |

---

## 7. 开放点（评审需拍板）

| # | 开放点 | 建议 |
| --- | --- | --- |
| 1 | 快照刷新频率 vs 决策新鲜度 | ✅ **已定**：价格 6h/余额 5~15min/健康准实时(每次 attempt 结束增量更新)/订阅 1h（参数10 默认，可配） |
| 2 | 用满倍率的"周期额度用满"口径 | ✅ **已定**：按 `period_quota` 全部用满计边际成本；未用满风险由倾斜三道闸的启动条件兜（§4.1） |
| 3 | 容量保留与订阅引流上限的叠加冲突 | ✅ **已定**：接管保留优先级最高，订阅倾斜不得侵占接管/金级保留（§4.2 显式排除） |
| 4 | 测活成本归因窗口 | ✅ **已定**：按 `requests.probe_kind='probe'` 关联到 `attempt_usage` 后滚动 30 天，与月费用比（§2.1） |

> 上述均为**一期已定取舍**（可配置项默认值 + 判断题结论），非待议；标 ✅ 以便读者区分"已定"与"待拍板"。

---

_本篇为 M2（RoutePlan/期限）与 M4（经营闭环）的策略基线。所有数值默认可配置（`config_params`），关键项修改二次确认（FR-115）；策略变更须回溯 DECISIONS 参数编号。_
