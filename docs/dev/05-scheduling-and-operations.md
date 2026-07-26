# 05 调度与经营策略（selector 排序 + steward 经营闭环 · v0.1 草案）

| 项目 | 内容 |
| --- | --- |
| 状态 | ✅ **v1.0 基线（2026-07-26 冻结）** —— 经 28 轮对抗性审查 + 2 轮开发视角走查 + PM 开工前裁决；变更须走版本记录 |
| 日期 | 2026-07-23 |
| 定位 | 把 `selector`（候选选择与排序）与 `steward`（测活/冷却/订阅倾斜/告警/错误预算）落成可实现规则。数值全部来自 [DECISIONS](../DECISIONS.md) 参数确认，**作默认值 + 可配置**（存 [02 §2 `config_params`](./02-data-model.md)，关键项二次确认，FR-115） |
| 输入 | [PRD v1.3](../PRD.md)、[DECISIONS](../DECISIONS.md)（16 参数）、[02 数据模型](./02-data-model.md)、[03 上游对接层](./03-upstream-layer.md)、[01 架构](./01-architecture.md)（selector/steward 模块） |
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
    SingleHopUSD        float64   // 本跳上界（供 closeout/recovery 估算该跳费用）
    ClaimIntent         string    // ''（无）| 'canary' | 'probe' —— 决定 dispatch 走哪个变体
    CachePredicted      float64   // FR-056 预测命中率，入 decision_snapshot
    CacheTarget         float64   // 对应目标值，入 decision_snapshot
}
```

**各字段的生成者**：`selector` 填全部——价格/倍率版本取自决策时的内存快照（这正是 FR-013「每请求关联决策时价格版本」的落点）；`executor` **只读不改**；`ledger` 从中取 dispatch 事务的参数。

要点：

- **每跳期限按 SLA 等级 TTFT 预算分配**：金级首跳期限 = min(该 binding 近期 P95, 等级 TTFT 预算)，为接管留余量（前缀平均 ≤10s → 金级单跳 P95≤5s，§11）。
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
  > 落到事务层：dispatch 链会分别返回 `quota_ok` / `canary_ok` / `advanced` 三值（[02 §2bis](./02-data-model.md)）。**`canary_ok=0` 必须 ROLLBACK 后按非 canary 候选重新 dispatch，不得当成 429** —— 二者混判会在 canary 并发竞争时拒绝本可服务的请求。
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
| 租户 | 单租户测活 ≤ 该租户月费用 **2%**；同租户同时 ≤ **1** 个测活请求 | 租户级预算计数器 + 并发闸 |
| 会话 | 每会话最多被测活 **1** 次；会话第一轮、金级会话默认不测活 | `session_prefix_ledger` / 会话级标记 |
| 错误预算 | 测活导致的失败 ≤ 该等级错误预算 **10%** | §5 错误预算扣减，超限停测活 |

> **五维限制（FR-063）**：上表列全局/租户/会话/错误预算；此外**用户级**与**资源级**同样分别设测活费用/次数/并发/错误预算上限（`config_params` 按 scope 分别配置）。

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

### 5.3 余额不足信号自适应识别（参数5/FR-027）

余额**非实时**、后台校对；置"耗尽"靠多判据组合（[02 `balance_signals`](./02-data-model.md)）：错误码 / 错误文案正则("余额"类关键词) / 真实请求失败信号 / 余量归零（AC-29）。保守储备与三档处置（<24h 告警 / <6h 停测活与高成本 / <1h 临界减普通流量，参数5）。

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
