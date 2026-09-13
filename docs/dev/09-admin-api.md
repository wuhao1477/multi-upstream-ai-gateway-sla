# 09 管理 API 与设置面（FR-115 落地 · v0.1 草案）

| 项目 | 内容 |
| --- | --- |
| 状态 | ✅ **v1.0 基线（2026-07-26 冻结）** —— 经 42 轮对抗性审查（含 5 轮开发视角）+ PM 开工前裁决；变更须走版本记录 |
| 日期 | 2026-07-23 |
| 定位 | 补齐设计审查发现的空缺：[02 `config_params`](./02-data-model.md) 有表有 `confirmed_twice` 列，但**无操作面**。本篇定义读写这些配置的**管理 API**，以及 FR-115 要求的"设置页明示含义/用法/影响 + 关键项二次确认" |
| 输入 | [FR-115/116](../PRD.md)、[DECISIONS 总原则](../DECISIONS.md)（策略参数"建议默认 + 设置页可改，关键项二次确认"）、[02 数据模型](./02-data-model.md)、[05 调度与经营策略](./05-scheduling-and-operations.md)（策略语义来源） |
| 范围 | **管理平面 API + 二次确认流程 + 策略元数据**。设置页 UI 本身一期可用最简形态（甚至 API + 文档即可），本篇只定契约 |

> **务实边界**：个人/内部使用，不追求华丽后台。一期目标是"**有一条安全、可审计、带二次确认的配置通道**"，而非完整运营控制台。UI 可后置，API/契约必须先定——否则 `config_params` 只能靠手改 SQL，二次确认（FR-115 硬要求）无从落地。

---

## 1. 管理平面与数据平面分离

| 平面 | 端点前缀 | 鉴权 | 说明 |
| --- | --- | --- | --- |
| 数据平面 | `/v1/*`（chat_completions、responses） | **网关调用方凭证**（`gateway_clients`，只存哈希） | 承载真实请求（[03](./03-upstream-layer.md)）。⚠️ **严禁复用 `upstream_keys.secret`**——那是打上游用的，复用会把高价值凭证暴露给调用方（[02 §2bis](./02-data-model.md)） |
| 健康 | `/healthz` | 无 | LB 探针（[06](./06-deployment-and-operations.md)） |
| **管理平面** | **`/admin/*`**、`/metrics` | **两层都要**：① 网络边界（Caddy 不代理，仅容器网络/本机可达）② **独立管理令牌** `ADMIN_TOKEN`（env 注入，非业务 Key，与 `gateway_clients` 无关） | 本篇；配置读写、策略、审计查询 |

> ⚠️ **鉴权口径冻结（第 31 轮：09 说"独立管理令牌"、06 说"网络边界是唯一边界"，两处打架，直接决定 handler 要不要验权、失败返回什么、env 要不要加）**：
> **两层都要,不是二选一**。理由：网络边界防的是外部;管理令牌防的是**同一台机器上的其它进程或容器**（compose 里还跑着 collector、caddy、postgres，它们都不该能改配置）。
> 一期不做的是**多用户/RBAC**（[15](./15-scope-and-preflight.md) 已确认仅本人使用），不是不做鉴权 —— 一把静态令牌成本几乎为零。
> 缺失或错误的令牌一律 **401**；令牌**不得**出现在日志与 `/metrics`（[12 §6](./12-debuggability.md) 脱敏同标准）。

- 管理平面挂在 sla-core 上，是**我方核心对运维暴露的面**；与上游站点自身的管理接口（采集器访问，[04](./04-collector-adapter.md)）互不相干。
- 一期鉴权：单个**管理令牌**（环境变量注入，明文一期可接受，随 FR-113 一起在对外前升级）；管理平面**只在内网/本机可达**，不经公网。

---

## 2. 配置读写 API（对应 [02 `config_params`](./02-data-model.md)）

| 方法 | 端点 | 作用 |
| --- | --- | --- |
| `GET` | `/admin/config` | 列出所有配置项（含 scope、当前值、是否关键、版本、元数据） |
| `GET` | `/admin/config/{scope_type}/{scope_id}/{param_key}` | 取单项（含默认值、含义、影响、历史版本） |
| `POST` | `/admin/config/preview` | **预演**：提交拟改值，返回校验结果 + 影响评估 + 是否需二次确认，**不落库** |
| `POST` | `/admin/config/apply` | 应用变更；关键项须带二次确认令牌（§3） |
| `GET` | `/admin/config/history?param_key=` | 变更历史（FR-099：前后值/责任人/原因/时间） |

- 所有写操作**只 INSERT 新版本**（`config_params` 的 `version+1`），不 UPDATE 旧行——与价格版本同源的不可覆盖原则，保证可回溯、可回滚（FR-104）。
- `apply` 成功后触发**内存快照重建**（[01 §5](./01-architecture.md)），使新配置对决策路径生效；决策路径本身仍只读快照、不查库。

---

## 3. 二次确认流程（FR-115 的核心，`is_critical=true` 强制）

关键策略（测活预算、禁测活规则、订阅倾斜、旁路开关、保留窗口缩短、SLA 指标等，`config_params.is_critical=true`）**必须两步提交**：

```
1 POST /admin/config/preview {param_key, new_value, changed_by, change_reason}
    → 200 {
        valid: true,
        is_critical: true,
        current_value, new_value,
        current_version: 7,              ← 预览时的版本，apply 必须原样带回
        impact: "该改动会把全局测活预算从 2% 降到 1%，预计每日探索次数 -50%…",  ← 明示影响(FR-115)
        confirm_token: "<一次性令牌，绑定本次 diff + 5min TTL>"
      }
2 POST /admin/config/apply {param_key, new_value, changed_by, change_reason,
                            confirm_token, expected_current_version: 7, idempotency_key}
    → 单事务内：锁定该参数当前最大版本 → 比对 expected_current_version
       → 不匹配返回 409（preview 已过期，须重新预览）
       → 匹配则分配 version=max+1、INSERT、confirmed_twice=true、消费 confirm_token → 200
```

- `confirm_token` 绑定**具体 diff**：改了值再 apply 会因 token 不匹配被拒——防"确认了 A 却提交了 B"。
- **并发控制（对抗性审查修正）**：仅靠 token 不够——两个基于同一旧值的 preview 可**都**通过校验，导致重复版本、多实例加载到不确定值。故：
  - `expected_current_version` 做**乐观锁**：两个并发 apply 只有一个成功，另一个 409；
  - `idempotency_key` 做**去重**：网络重试重复投递只生效一次（[02 `config_params` UNIQUE(param_key, idempotency_key)](./02-data-model.md)）；
  - 比对、分配版本、插入、消费令牌**必须在同一事务**，否则窗口期仍可并发插入。
  - 全局参数的 `scope_id` 用哨兵值 `'*'` 而非 NULL——PG 普通 UNIQUE 允许多行 NULL，用 NULL 会让唯一约束失效（[02](./02-data-model.md)）。
- 非关键项（`is_critical=false`）可跳过 preview 直接 apply（仍记审计）。
- **对应 [02 列](./02-data-model.md)**：`confirmed_twice` 唯有走完二次确认才为 `true`；决策路径加载配置时可断言"关键项 `confirmed_twice=true` 才生效"，未确认的关键改动不进快照。

---

## 4. 策略元数据（FR-115"明示含义/用法/影响"）

每个策略参数在代码里带静态元数据（随二进制发布，非用户可改），供 `GET /admin/config` 与设置页渲染：

```go
type ParamMeta struct {
    Key         string   // "probe.budget.global_pct"
    Group       string   // "测活" / "SLA" / "订阅" / "容量" / "告警" …
    Title       string   // "全局测活预算"
    Description string   // 一句话含义
    Unit        string   // "%" / "秒" / "次"
    Default     any      // 建议默认值（DECISIONS 参数值）
    Critical    bool     // 是否关键 → 二次确认
    ImpactHint  string   // "调低会减少探索、可能延缓渠道评估更新"
    SourceRef   string   // "DECISIONS 参数3 / FR-063 / 05 §2.1"  ← 每项可回溯
}
```

- 元数据**逐项回溯到 DECISIONS 参数号或 FR**（`SourceRef`），杜绝"设置页有个开关但没人知道它对应哪条需求"。
- 一期设置页可以只是：`GET /admin/config` 的 JSON + 一个极简表单页（甚至 curl 脚本），把 `Description/ImpactHint/二次确认`落到位即可满足 FR-115；美化后置 M4。

---

## 4bis. P1 `config_params` 键清单（**权威来源**）

> 此前配置键散落在 01/02/03/05/06/14 十余处，**没有一份清单** —— 开发不知道迁移种子该初始化哪些、`/admin/config` 该校验哪些、漏掉一个只会在运行时以默认零值的形式静默出错。
> **本表是唯一权威来源**：新增键必须先进本表。**一行一个完整 `param_key`，不得用 `a / b` 合并行或 `.x` 缩写**——迁移种子与 `/admin/config` 白名单都按本表逐行生成，合并行会漏键。`is_critical=true` 的走 [§3 二次确认](#3-二次确认流程fr-115-的核心is_criticaltrue-强制)。

### 4bis.0 取值路径（第 40 轮补：清单有了，**怎么读出一个值**从没写过）

> ⚠️ `config_params.param_value` / `prev_value` 建表后全库零引用。下方清单列全了键、
> §4 定义了静态元数据，但"给定一个 key，运行时拿到哪一行、什么类型"没有任何交代 ——
> 而全套设计里几乎每条规则都以"（`config_params` 可配）"收尾。

**生效行的选取**（后台刷快照时执行，同步路径只读内存，FR-110）：

```sql
-- 某 scope 下某键的当前生效值
SELECT param_value FROM config_params
 WHERE scope_type = :scope_type AND scope_id = :scope_id AND param_key = :key
   AND effective_at <= now()
   AND (NOT is_critical OR confirmed_twice)   -- 关键项未二次确认 → 视同未生效
 ORDER BY version DESC LIMIT 1;               -- 只 INSERT 不 UPDATE，版本号最大者生效
```

**作用域回退顺序**（先具体后宽泛，命中即止）：

```
model:<model_id> → channel:<channel_id> → policy:<policy_id> → tenant:<tenant_id> → global:'*' → ParamMeta.Default
```

- **`global` 的 `scope_id` 恒为 `'*'`**（[02](./02-data-model.md) 的 CHECK 约束），查询时**不得**传 NULL。
- **兜底是代码里的 `ParamMeta.Default`，不是零值**：库里查不到该键属正常（迁移种子只初始化非默认项），
  但**绝不能因此得到 0** —— 一个默认 0 的预算比例会让整套测活静默停摆。
- **未二次确认的关键项按"没改过"处理**：回退到上一版本或默认值，而不是拒绝服务。

**类型**：`param_value` 是 `JSONB`，**按 [§4 `ParamMeta`](#4-策略元数据fr-115明示含义用法影响) 定型**——
比例存 JSON number（`0.02`，不是 `"2%"`）、时长存整数毫秒/秒并以 `_ms`/`_sec` 后缀自明、开关存 JSON boolean。
**转换只发生在应用层**（[00 §5.1](./00-overview-and-milestones.md) 已列为实现期须确认的假设之一），SQL 里不做 `::numeric` 强转——
类型不符应在 `/admin/config/apply` 时按 `ParamMeta` 校验并 400，而不是留到运行时炸。

**`prev_value` 的写入点**：`apply` 插入新版本行时，把**当时生效行**的 `param_value` 抄进新行的 `prev_value`
（FR-099 要的是"前后值"可查）；首次设置该键时为 NULL。它是审计字段，**不参与取值**。

| 键 | 默认 | 关键项 | 用途 / 定义处 |
| --- | --- | --- | --- |
| **采集** ||||
| `collector_request_interval_ms` | 200 | | 站内请求间隔 |
| `collector_price_interval_h` | 6 | | 价格采集周期（小时） |
| `collector_balance_interval_min` | 5 | | 余额采集周期（分钟） |
| `collector_keyquota_interval_min` | 30 | | Key 额度采集周期（分钟） |
| `collector_catalog_interval_h` | 12 | | 渠道模型目录采集周期（小时，FR-126） |
| `catalog_missing_rounds` | 3 | | 模型连续 N 轮未出现即判下架（FR-126/AC-40） |
| `sync_min_interval_s` | 60 | | 同渠道手动 sync 的最小间隔（秒，FR-128） |
| **管理鉴权** ||||
| `admin_token` | 由 env `ADMIN_TOKEN` 注入 | ✅ | 管理面令牌，**不落 `config_params`** |

**CI 断言**：迁移种子为七个数据库配置键插入行；`admin_token` 只来自环境；`/admin/config` 拒绝写入表外键。

## 4ter. 后续阶段配置设计（非 P1 运行白名单）

> 下表仅保留 P2/P3/P4 设计语义，不生成代码、不种子、不被 P1 管理 API 接受。

| 键 | 默认 | 关键项 | 用途 / 定义处 |
| --- | --- | --- | --- |
| **调度与期限** ||||
| `default_ttft_budget_ms` | 8000 | | 无承诺策略的 TTFT 预算（[05 §1.3](./05-scheduling-and-operations.md)） |
| `prefix_target_ms` | 10000 | | 连续会话前缀平均目标 |
| `ttft_safety_margin_ms` | 500 | | 逐跳分配的安全余量 |
| `min_hop_ms` | 1500 | | 单跳最小期限，低于此不再排跳 |
| `max_hops` | 3 | | RoutePlan 最大跳数 |
| `price_stale_hours` | 48 | | 价格新鲜度阈值 |
| `snapshot_refresh_ms` | 1000 | | 内存快照刷新周期 |
| **缓存（FR-056）** ||||
| `cache_switch_min_hit_rate` | 0.6 | | 切换抑制阈值（**内部参数，非对外承诺**，[02 §6quater](./02-data-model.md)） |
| **canary（FR-121）** ||||
| `canary_max_per_hour` | 20 | | per-binding 每小时上限 |
| `canary_max_concurrent` | 1 | | per-binding 并发上限 |
| `canary_failure_threshold` | 3 | | 连续失败退回 cooling |
| **主动测活（FR-060~067）** ||||
| `probe_idle_window_min` | 30 | | 多久无业务流量才发探测 |
| `probe_batch_size` | 3 | | 每轮最多探几个 binding |
| `probe_global_cost_cap_ratio` | 0.02 | ✅ | 全局测活费用 ≤ 月费用 2% |
| `probe_binding_daily_cap` | 20 | | 单 binding 日探测次数 |
| `probe_tenant_cost_cap_ratio` | 0.02 | ✅ | 单租户测活 ≤ 该租户月费用 2%（`:tenant_cap` = 该租户月费用 × 本值） |
| `probe_user_cost_cap_ratio` | 0.02 | ✅ | 单调用方同上（一期 user≡client，`:user_cap` 同源） |
| `probe_session_cap` | 1 | | 每会话最多被测活次数（`:session_cap`） |
| `probe_error_budget_ratio` | 0.10 | ✅ | 测活失败占错误预算上限 |
| **容量保留（FR-029/032）** ||||
| `capacity_ceiling_normal` | 0.65 | | 四类流量的并发天花板（[02 §6bis-2](./02-data-model.md)） |
| `capacity_ceiling_committed` | 0.85 | | |
| `capacity_ceiling_takeover` | 0.95 | | |
| `capacity_ceiling_probe` | 0.70 | | |
| `balance_safety_reserve_ratio` | 0.02 | | 余额安全储备比例（[05 §5bis.2](./05-scheduling-and-operations.md) 的 safety_reserve） |
| `balance_text_patterns` | `["余额","额度","欠费","insufficient","quota"]` | | 余额不足文案正则关键词表（[05 §5.3](./05-scheduling-and-operations.md)）；各中转站文案不同，按历史 `signal_evidence` 调 |
| `balance_safety_reserve_min_usd` | 1.0 | | 安全储备下限，实际取 max(余额×比例, 本值) |
| `balance_hours_warn` | 24 | | **可持续时间三档（FR-030）**，[05 §5.3](./05-scheduling-and-operations.md)：低于本值告警 P2 |
| `balance_hours_stop_probe` | 6 | | 低于本值：停主动测活与高成本请求（该账号 binding 退出 probe 候选） |
| `balance_hours_critical` | 1 | | 低于本值：置 `balance_state='critical'` → 由 §1.1bis 判据 1 排除出候选 |
| `retention_months` | 7 | ✅ | 账本分区保留窗口（≥180 天，FR-112） |
| **健康与冷却（参数 11）** ||||
| `health_min_samples_1h` | 20 | | 样本门槛 |
| `health_min_samples_24h` | 100 | | |
| `cooldown_base_sec` | 300 | | 冷却退避起点 |
| `cooldown_max_sec` | 14400 | | 冷却退避上限 |
| `observing_min_minutes` | 30 | | 观察期时长（先到为准） |
| `observing_min_success` | 50 | | 观察期连续成功数 |
| **故障域（FR-045，[05 §5bis.1bis](./05-scheduling-and-operations.md)）** ||||
| `domain_min_bindings` | 3 | | 少于此数的故障域不做集中失败判定（避免单渠道误封整域） |
| `domain_fail_ratio` | 0.6 | | 域内 cooling/degraded 占比超此值即封禁整域 |
| `domain_disable_sec` | 600 | | 自动封禁时长；到期自动解封，靠下一轮重新判定续期 |
| **告警生命周期（[05 §5.2bis](./05-scheduling-and-operations.md)）** ||||
| `alert_close_after_sec` | 900 | | `recovering` 持续多久自动 `closed` |
| **配额与计费** ||||
| `billing_variance_tolerance` | 0.05 | ✅ | 计费偏差容差（[05 §4.4](./05-scheduling-and-operations.md)） |
| `billing_min_base_usd` | 0.01 | | 低于此改用绝对差额判定（除零保护） |
| `billing_abs_tolerance_usd` | 0.05 | | |
| `billing_recovery_streak` | 20 | | 连续一致次数后恢复 |
| **告警** ||||
| `alert_webhook_url` | `""` | ✅ | 为空则不外发（[06 §5bis](./06-deployment-and-operations.md)）。⚠️ 默认值写作 `""` 而非"空"字样：种子按字面量入库，写"空"会让 P3 的 webhook 代码拿到一个名为"空"的 URL 去 POST，而不是识别为未配置 |
| `alert_recovery_checks` | 3 | | 判据连续几轮不成立才转 `recovering`（[05 §5.2bis](./05-scheduling-and-operations.md) 生命周期表） |
| `key_invalid_streak` | 3 | | 连续 401/403 判 Key 失效 |
| `all_unavail_checks` | 2 | | 连续几轮空候选判全不可用 |
| `error_budget_burn_ratio` | 0.5 | | 快速消耗阈值 |
| `stale_alert_hours` | 12 | | 数据过期告警 |
| **采集** ||||
| `collector_request_interval_ms` | 200 | | 站内请求间隔 |
| `collector_price_interval_h` | 6 | | 价格采集周期（小时） |
| `collector_balance_interval_min` | 5 | | 余额采集周期（分钟） |
| `collector_keyquota_interval_min` | 30 | | Key 额度采集周期（分钟） |
| `collector_catalog_interval_h` | 12 | | 渠道模型目录采集周期（小时，FR-126）。比价格稀疏——目录变动频率远低于价格 |
| `catalog_missing_rounds` | 3 | | 模型连续 N 轮未出现即判下架并告警（FR-126/AC-40）。⚠️ **用轮数而非时长**：采集周期本身可配，轮数对周期变化免疫。**缺此键会静默取 0 → 每轮都判下架**（第 45 轮补，AC-40 早已声称"可配、默认 3"却从未登记） |
| `sync_min_interval_s` | 60 | | 同渠道手动 sync 的最小间隔（秒，FR-128）。间隔内再调返回 429 且不打上游；**窗口只由触达过上游的尝试起算**，前置失败返 422 不占窗口（[§5.0bis](#50bis-sync-的编排规范p1-核心端点第-45-轮补)） |
| **执行面（第 31 轮补：以下键被 02/03/12 引用但未进本表，而本表会拒绝表外键 → 直接 400）** ||||
| `takeover_buffer_max_bytes` | 262144 | | T2 缓冲上限，达到即强制提交（[15 T2](./15-scope-and-preflight.md)、[03 §3.5](./03-upstream-layer.md)） |
| `takeover_buffer_max_ms` | 5000 | | 同上，时间维 |
| `cancel_cost_safety_ratio` | 1.3 | | 取消成本的**安全系数**（[02 §4](./02-data-model.md)：旁路观测字节 ÷2 得到的是 token **下界**，故乘该系数上浮）。⚠️ 第 33 轮修正：此前登记为 `cancel_cost_safety_usd=0.05`，把**系数**误记成**金额**，两者语义完全不同 |
| `tenant_probe_concurrency` | 1 | | 同租户在飞探测数上限（[05 §2.1bis](./05-scheduling-and-operations.md) 第六维闸） |
| `debug_capture_enabled` | `false` | ✅ | L2 抓包总开关（[12 §3](./12-debuggability.md)）——**含正文，生产默认关** |
| `debug_capture_max_requests` | 20 | | 抓包环形上限 |
| `debug_capture_max_bytes` | 1048576 | | 单请求抓包上限（1MB），超出截断 |
| `debug_capture_ttl_minutes` | 30 | | 抓包自动过期，防长期驻留正文 |
| **开关** ||||
| `data_policy_enabled` | `false` | ✅ | 数据许可硬过滤（[02 §2ter](./02-data-model.md)） |
| `alias_passthrough` | `false` | ✅ | 未命中别名时按同名直连（**绕过别名策略**，仅迁移期用） |
| `load_test_mode` | `false` | ✅ | 输出 `x-sla-*` 内部时延头（**生产不得开**，[14 §2ter](./14-acceptance-matrix.md)） |
| `ledger_batch_commit_enabled` | `false` | | 组提交总开关（[14 §2ter](./14-acceptance-matrix.md)） |
| `ledger_batch_commit_max_size` | 16 | | 单事务最多合并几个请求 |
| `ledger_batch_commit_linger_ms` | 2 | | 攒批等待上限，**不得超 2ms** |
> 该历史表不参与 P1 CI 与运行配置。

---

## 5. 其余管理端点（P1~P3 逐步）

> **阶段列读法**（[PRD §2.1.0](../PRD.md)）：**P1** = 上游采集与管理；**P2** = 原 M1+M2（网关核心）；**P3** = 原 M3+M4（调度与经营）。下表原有的 M0~M4 标记继续有效，P1 行为本轮新增。

### 5.0 P1 端点：上游资产采集与管理（FR-122~128）

> 这 11 个端点构成 P1 的**全部对外面**（管理平面）。P1 **不新增任何 `/v1/*` 端点**。

| 端点 | 作用 | 阶段 |
| --- | --- | --- |
| `GET /admin/channels`、`POST /admin/channels`、`PATCH /admin/channels/{id}` | 渠道 CRUD。此前只能经 `/admin/bindings` 间接建渠道，无独立管理面 | **P1** |
| `GET /admin/channels/{id}/inventory` | **资产总览**：账号数 / Key 数 / 分组数 / 目录模型数 / 额度合计 / 最近同步时刻 / **异常项计数**（FR-128 展示面、FR-129 并入）。<br>**异常项的构成（第 45 轮定义——它被三处引用却从未定义）**：① 数据陈旧（`fetched_at` 超对应 `collector_*_interval` 的 2 倍）；② `Degraded` 结果的 `MissingFields` 待人工补录（[04 §3.4bis](./04-collector-adapter.md)）；③ 上游存在但库中未登记的 Key（[02 §1.3bis](./02-data-model.md)）；④ 疑似下架模型（`catalog_missing_rounds` 已达阈值）；⑤ 凭证状态非 `valid`（`collector_credentials.status`）；⑥ Key 状态非 `active` 或已过期。**逐类给出计数与可下钻的列表**，不合并为一个总数——否则运维看到"异常 7"却不知道该修什么 | **P1** |
| `POST /admin/channels/{id}/sync` | **手动立即刷新**（FR-128）。**编排规范见 [§5.0bis](#50bis-sync-的编排规范p1-核心端点第-45-轮补)** —— 顺序、事务边界、部分失败语义、响应结构、限流缺一不可实现；与周期采集共用同一 Runner | **P1** |
| `GET /admin/accounts`、`POST /admin/accounts`、`PATCH /admin/accounts/{id}` | 账号 CRUD（`external_user_id`、`balance_group_key`、停用列）。列表行还带**该账号采集凭证的存在性与形态**：`cred_type` / `cred_status` / `cred_expires_at`，**绝不含令牌内容**（FR-094）。凭证与账号是 1:1（`collector_credentials` 上的 `UNIQUE(account_id)`，[023 迁移](../../migrations/023_account_credentials.sql)），所以它是账号行的一列而不是另一种资源；`cred_type` 缺席 = 没登记 = 这个账号采不了。⏭ 充值倍率 `topup_rate` 属 P3，本阶段不提供 | **P1** |
| `GET /admin/keys`、`POST /admin/keys`、`PATCH /admin/keys/{id}` | 上游 Key CRUD（FR-122）。**明文只在 `POST`/`PATCH` 请求体中接收，响应与列表一律只回 `secret` 前缀**（FR-094）；PATCH 替换 `secret` 即完成轮换；可设 `channel_group_id` | **P1** |
| `POST /admin/keys/import` | 按指定渠道或账号读取上游已有 Key 并登记；`deferred` 统计未读取的 Key，`deferred_accounts` 统计因明文读取预算未处理的账号。**范围字段**：`channel_ids[]` / `account_ids[]`（界面用的多选形状）与旧的单数 `channel_id` / `account_id` 等价 —— 服务端把单数归一进复数，下游只看复数；两者都缺即 `400`（本端点不接受无范围调用，`all` 这一位对它无效） | **P1** |
| `POST /admin/keys/provision?dry_run=true\|false` | 按账号的全部分组或指定模型分组补齐远端 Key；可筛选仅处理没有任何远端 Key 的账号。执行前必须预览，服务端限制单次创建与明文读取数量。范围字段同上；不给任何范围时必须显式 `all=true` 且 `only_without_keys=true` | **P1** |
| `POST /admin/keys/{id}/disable` | Key 停用（FR-122，承 FR-004/095 的 Key 层） | **P1** |
| `GET /admin/keys/{id}/usage?from=&to=` | Key 用量历史（FR-125）：读 `collector_snapshots(scope_type='key')` 的 payload 时序，返回剩余/已用/请求数曲线 | **P1** |
| `GET /admin/channel-groups?channel_id=` | 分组列表含 `group_ref`、`rate_multiplier`、可用模型数、`fetched_at`（FR-123） | **P1** |
| `GET /admin/channel-groups/{id}/models` | 该分组可获取的模型清单（FR-124），即"这把 Key 能用哪些模型"的答案 | **P1** |
| `GET /admin/channels/{id}/catalog?stale=&q=` | 渠道模型目录（FR-126）：分页 + 按价格排序 + 按名称筛；`stale=true` 筛出 `last_seen_at` 停止更新的**疑似下架**模型 | **P1** |
| `GET /admin/site-families` | **已注册的站型**：读 [04 §7bis](./04-collector-adapter.md) 的站型注册表，逐项返回 `family`/`display_name`/`aliases`/`cred_type`/`requires_external_user_id`。存在的理由是界面的站型下拉此前写死四项——**加一个站型时那份写死的列表不报任何错**，新站型只是在界面上不存在，运维只能靠自动探测碰上它。不查库、不碰凭证 | **P1** |
| `GET /admin/hub-sync`、`PUT /admin/hub-sync` | all-api-hub 的 **WebDAV 定时同步**配置（单行，`hub_sync_config`）。响应**只回 `has_webdav_password` / `has_backup_password`，不回显任一密码**（FR-094 同源纪律）；写入时这两个字段**留空 = 保持原值**，不是清空 —— 界面上它们每次打开都是空的，若当清空，任何一次"只改间隔"的保存都会把密码抹掉，而症状要等下一轮同步 401 才出现。`apply_mode` 取 `report`（只拉取比对）或 `import`（等同正式导入）；`interval_minutes` 下限 5。⚠️ WebDAV 地址**刻意不过 SSRF 校验**：自建 WebDAV 十有八九就在内网，而这个地址是管理员在鉴权之后自己填的，与导入文件里那一百个陌生站点不是同一类输入 | **P1** |
| `POST /admin/hub-sync/run?apply=true\|false` | 立即跑一轮：取回 WebDAV 上的备份 → 解密（上游信封为 PBKDF2-SHA256 + AES-256-GCM，见 [04](./04-collector-adapter.md)）→ 走 `/admin/import/all-api-hub` 同一条管线。`apply=true` 强制落库，否则按 `apply_mode`。定时器跑的是同一段代码，只是永远不 force | **P1** |
| `GET /admin/collector/credentials`、`POST /admin/collector/credentials` | 采集凭证读写（[04](./04-collector-adapter.md)、明文一期；响应只报状态与是否存在，不回显内容）。⚠️ 管理界面**只用 `POST`**：凭证是账号的属性，列表那一侧已由 `GET /admin/accounts` 的 `cred_*` 三列覆盖（2026-09-13 并栏）。`GET` 保留给脚本 | **P1** |

### 5.0bis `sync` 的编排规范（P1 核心端点，第 45 轮补）

> ⚠️ **此前只有一句"依次跑五个 Fetch、返回逐项结果"** —— 顺序依据、事务边界、第 3 项失败时前 2 项是否回滚、响应长什么样、并发点两次会怎样，**全部未定义**。这是 P1 最核心的端点，开发第一天就会卡在这里（开发视角审查第 45 轮）。

**执行顺序（有依赖，不可交换）**：

```text
① Authenticate            —— 失败即整体中止（后续全部依赖会话句柄）
② FetchAccount            —— 产出 external_user_id，NewAPI 系后续请求的头部必需
③ FetchGroups             —— 必须先于 ④：Key 要挂 channel_group_id，分组行得先存在
④ FetchKeys               —— 写 upstream_keys 的用量列 + channel_group_id
⑤ FetchPricing            —— 写 price_versions（不可覆盖版本）
⑥ FetchModelCatalog       —— 写 channel_model_catalog
```

- **①② 是硬前置**，失败则整体返回 `502` 并**不写任何表**（连不上或认不过，谈不上采集）。
- **③ 必须先于 ④**：`upstream_keys.channel_group_id` 是外键指向 `channel_groups`，反序会拿不到 id。
- **⑤⑥ 相互独立**，可并发；但为限流简单起见 P1 串行执行。

**事务边界：逐项独立提交，不做跨项大事务**：

| 项 | 事务粒度 | 失败影响 |
| --- | --- | --- |
| ③ 分组 | 一个事务（含 `group_models` 全量替换，见 [02 §1.3bis](./02-data-model.md)） | 只该项标 `failed`，④ 仍可跑（Key 的 `channel_group_id` 留空并记 warning） |
| ④ Key | **每把 Key 一个事务** | 单把 Key 失败不影响其它 Key |
| ⑤ 价格 | 一个事务（每个模型一条 `price_versions` INSERT） | 只该项 `failed` |
| ⑥ 目录 | 一个事务（upsert 全量，见 [02 §1.3bis](./02-data-model.md)） | 只该项 `failed` |

- **为何不用一个大事务**：一次 sync 可能写数百行（200+ 目录模型 + 数十把 Key），单事务会长时间持锁；且"价格采到了但目录超时"时**没有理由把价格也丢掉**——采集是幂等的补齐动作，不是要么全有要么全无的账务操作。
- **每项都在自己事务里写一条 `collector_snapshots`**（[02 §7.1](./02-data-model.md) 的 payload 结构），保证"这次采到什么"与业务表同生共死。

**响应结构**（`200` 表示"编排完成"，逐项成败看 `items`）：

```json
{
  "channel_id": 7, "site_family": "newapi", "started_at": "…", "elapsed_ms": 4210,
  "items": [
    {"capability":"account","status":"ok","elapsed_ms":210,"rows":1},
    {"capability":"groups","status":"ok","elapsed_ms":180,"rows":3},
    {"capability":"keys","status":"partial","elapsed_ms":900,"rows":3,
     "failed":1,"error":"key 12: 401 unauthorized","http_status":401},
    {"capability":"pricing","status":"ok","elapsed_ms":760,"rows":214},
    {"capability":"model_catalog","status":"ok","elapsed_ms":2160,"rows":214}
  ]
}
```

- `status` 枚举：`ok` / `partial`（多账号采集部分成功，已保存可用账号结果并保留失败信息）/ `failed` / `unsupported` / `skipped`（**未打上游就跳过**：被限流 429、互斥 409，或前置条件不满足 422）。
- 上游 HTTP 失败时可附 `http_status`；存在有效 `Retry-After` 时同时附 `retry_after_ms`，供周期采集调度退避使用。
- P1 不支持的能力应在其所属后续阶段实现，不作为本期 `items` 占位项；P1 五项能力必须按 `supported`/`degraded` 规则返回。
- **`supported`/`degraded`/`unsupported` 的判定**：见 [04 §3.4bis](./04-collector-adapter.md)。`supported` 空结果判 `failed`；`degraded` 可返回部分数据或空结果，但必须在 `note` 说明；`unsupported` 显式返回，不留空。

**限流与并发**：

- **同渠道最小间隔** `sync_min_interval_s`（默认 60，`config_params`）：间隔内再次调用返回 **429** 且 `items` 全为 `skipped`，不打上游。
- ⚠️ **窗口只由"真的触达了上游"的尝试起算**：站型未知、连接池取不到连接、**凭证未登记**都在发出第一个上游请求前失败，此类返回 **422**（配置问题，非上游故障）且 `items` 全为 `skipped`，**不起算窗口**。否则「建渠道 → 采集 → 提示缺凭证 → 登记 → 再采集」这条首跑路径会被自己上一次的失败挡满一个间隔（[P1-evidence §4 第 15 项](../acceptance/P1-evidence.md)）。采集层用 `collector.ErrPrecondition` 显式声明"未触达上游"，**接口层不靠匹配错误文案判断**。
- **同渠道互斥**：用 `pg_try_advisory_lock(hashtext('sync:'||channel_id))`；抢不到锁返回 **409**（已有一次 sync 在跑）。**不排队**——手动刷新重复点击应立即得到反馈，而非静默堆积。互斥与限流是两件事：**前置失败也必须解互斥**，否则一次本地失败会把渠道永久锁死。
- 单项内的请求间隔仍受 `collector_request_interval_ms` 约束（[04 §6](./04-collector-adapter.md)）；`sla-core` 与 `collector` 通过 PostgreSQL host 时隙跨进程共享该限制。

### 5.1 其余端点（P2~P3）

| 端点 | 作用 | 里程碑 |
| --- | --- | --- |
| `GET /admin/bindings`、`POST /admin/bindings` | 渠道/绑定登记（落 `channels`/`upstream_keys`/`bindings`，含 `rpm_limit`/`concurrency_limit` 容量登记与 `enabled` 停用开关）。⚠️ **列表与详情须显式标注「未登记容量 → 保留未生效」**（[05 §4.3](./05-scheduling-and-operations.md)），供上游对接层读取，[03](./03-upstream-layer.md)）。**只引用已登记的 `model_id`，不在此隐式创建模型**——模型走 `/admin/models`。⚠️ **同事务写 `binding_fault_domains`**：按该渠道的 `channels.upstream_provider_id` 挂一条 `kind='provider'` 的边（域不存在则先建 `fault_domains`），另按 `region` 挂 `kind='region'` 边。**不自动挂就永远是空表**，[05 §1.2bis](./05-scheduling-and-operations.md) 的接管故障域去重与 [§5bis.1bis](./05-scheduling-and-operations.md) 的集中失败检测双双失效（两者都 JOIN 这张表） | M1 |
| `GET /admin/models`、`POST /admin/models` | **模型登记（唯一写入载体）**：`canonical_name` + **必填** `max_input_tokens`/`max_output_tokens`（[02 §1.1](./02-data-model.md)）+ 能力位。⚠️ 两个上界是**费用预留上界算法的硬前置**（[02 §2bis](./02-data-model.md)），缺任一即该模型的所有 binding **不进候选** → 接口层强制校验 `NOT NULL AND > 0`，缺失直接 **400**，不允许留空建模型 | **M1** |
| `PATCH /admin/models/{id}` | 更新上界与能力位（上界变更影响预留额，走 §3 二次确认） | M1 |
| `GET /admin/aliases` | 模型别名 ↔ 策略映射（[02 §2](./02-data-model.md)、FR-062） | M1 |
| `GET /admin/policies`、`PATCH /admin/policies/{id}` | 策略读写。⚠️ **原地更新主行**（`version+1`）**并同事务向 `routing_policy_revisions` 追加改前快照**——主表单行身份不可变，两个外键（`model_aliases.policy_id`/`requests.policy_id`）依赖它（[05 §1.0](./05-scheduling-and-operations.md)） | M1 |
| `GET /admin/policies/{id}/revisions` | 该策略的变更史（FR-104：版本/生效时间/变更原因/责任人）。按 `routing_policy_revisions.superseded_at` 倒序返回——该列是"这一版被改走的时刻"，与主行的 `effective_at`（当前版本何时生效）配对读，才能还原每一版的**生效区间** | M1 |
| `POST /admin/policies/{id}/rollback` | **恢复到上一个已确认版本**（FR-104 明文要求）。入参 `to_version`；把该 revision 的字段抄回主行、`version+1`，并把**回滚前的当前值**也存一条 revision——回滚本身也是一次变更，必须可再回滚 | M1 |
| `GET /admin/channel-models`、`POST /admin/channel-models` | **渠道×模型×协议能力矩阵的写入载体**（`channel_models`）。登记 `enabled`、探测结果（`support`/`supports_streaming`/`supports_tools`/`probed_at`），以及 ⚠️ **`upstream_model_name`——该渠道对这个模型的叫法**。中转站把 `gpt-5.5` 叫成 `openai/gpt-5.5` 或 `gpt-5.5-0930` 是常态，**不填就按 `models.canonical_name` 发出去，上游必然 404**（[05 §1.0 出站改写](./05-scheduling-and-operations.md)）。留空 = 与 `canonical_name` 相同 | **M1** |
| `GET /admin/ledger/requests?…` | 账本**列表**查询（按时间/client/别名/终态过滤；FR-097/098） | M1 |
| `GET /admin/ledger/requests/{request_id}` | 账本**详情**：该请求的全部 attempt、逐跳 usage 与成本、`decision_snapshot`、reservation 状态与终态时间线（[AC-16](./14-acceptance-matrix.md) 的判定入口）。**不含正文**（FR-112） | M1 |
| ~~`GET /admin/subscriptions`~~ | ⏭ **二期**：订阅台账/双倍率/到期浪费预测随订阅制整体推迟（[PRD §2.1](../PRD.md)）。**一期不提供该端点**；若为兼容预留，只允许返回稳定的 `{"error":"not_supported_in_phase_1"}`，**不得实现任何订阅查询、预测或双倍率逻辑** | ⏭ 二期 |
| `GET /admin/alerts` | 告警事件流（[02 §8](./02-data-model.md)、参数16）；默认只返回 `state <> 'closed'`，`?include_closed=true` 查历史 | M3 |
| `POST /admin/alerts/{id}/ack` | 人工确认（置 `state='acknowledged'` + `acknowledged_at`）。⚠️ **不阻断自动关闭**——判据恢复后照样走 `recovering → closed`（[05 §5.2bis](./05-scheduling-and-operations.md)） | M3 |
| `GET /admin/health` | 各 binding 健康/冷却/样本（[02 §6](./02-data-model.md)）；`?by=model\|request_type\|context_bucket` 走 `health_metric_windows` 的分维度行（FR-041），缓存作用域以 `cache_scopes.scope_label` 显示 | M2 |
| `GET /admin/debug/trace/{request_id}` | **L1 事件轨迹查询**（[12 §3](./12-debuggability.md)）：返回该请求各 attempt 的 SSE 事件元数据序列（`seq`/`offset_ms`/`event_type`/`should_commit`/`has_ttft_output`/`bytes`），**不含正文**（FR-112） | M1 |
| `GET /admin/ledger/reconciliation?scope=account\|key\|model&from=&to=` | **计费对账**（FR-019）：聚合预估 vs 实扣 vs 余额变化，不可归因差额单列 `unattributed` | M3 |
| `GET /admin/prices/changes?model_id=&channel_id=&key_id=&from=&to=` | **价格变化记录与影响范围**（FR-017）：查 `price_change_log`，逐条给出前后两版单价（join `from_version_id`/`to_version_id`）、`direction`、是否已确认，以及**影响范围**——该 `(channel, model)` 下受影响的 binding 列表与变更后窗口内的实际用量金额。三个过滤参数对应 FR-017 的「按模型、渠道和 Key 查询」（`key_id` 经 binding 反查） | M3 |
| `POST /admin/prices/changes/{id}/confirm` | 人工确认一条 `direction='decrease'` 的降价。⚠️ **必须在同一事务里改两张表**：`price_change_log.confirmed=true` **以及**该行 `to_version_id` 指向的 `price_versions.confirmed=true`（+ `confirmed_by`/`confirmed_at`）。selector 的「当前价」判据读的是 **`price_versions.confirmed`**（[02 §3](./02-data-model.md) 的 `idx_price_cur` 部分索引），只改留痕表**等于没确认**，该降价永远不参与低价优选，AC-03「确认后逐步增加」无法成立（[04 §价格变更留痕](./04-collector-adapter.md)、FR-014/AC-03） | M3 |
| `GET /v1/models`、`GET /v1/models/{alias}` | **数据面**端点，非管理面。由网关**合成**（[03 §4](./03-upstream-layer.md)）：返回该凭证 `allowed_aliases` 内的启用别名；非别名 404、越权 403；`x-models-etag` 我方自生成并支持 `If-None-Match` → 304 | M1 |
| `POST /admin/bindings/{id}/canary` | 把 binding 置回 `canary` 态并重置窗口计数，用于新渠道受控验证（[05 §2.0](./05-scheduling-and-operations.md)） | M2 |
| `GET /admin/fault-domains` | 列出故障域（`kind` + `label` 作显示名）及其当前封禁态、域下 binding 数与健康分布（[02 §1.2](./02-data-model.md)） | M3 |
| `POST /admin/fault-domains/{id}/disable` | **人工隔离整个故障域**：置 `disabled_until`（入参 `duration_sec`，**省略 = NULL = 无限期，须人工恢复**）+ `disabled_reason`。用于已知供应商维护窗口等自动判据覆盖不到的场景（[05 §5bis.1bis](./05-scheduling-and-operations.md)） | M3 |
| `POST /admin/fault-domains/{id}/enable` | 解除封禁（置 `disabled_until=NULL`）。⚠️ 若集中失败仍在持续，下一轮健康聚合会**再次自动封禁**——这是预期行为，不是解封失败 | M3 |
| ~~`POST /admin/collector/credentials`~~ | 已前移到 P1，见 §5.0 | P1 |
| `POST /admin/clients` | **签发网关调用方凭证**：生成随机明文 → 存哈希 → **明文只返回一次**；可设 `allowed_aliases`/`quota_daily_usd`（NULL=不限额）/`rpm_limit`（**NULL=不限速**，此时跳过 RPM 闸）/`expires_at`/**数据许可属性 `tenant_id`/`region`/`business_tier`/`data_class`**（[02 §2bis](./02-data-model.md)） | **M0** |
| `GET /admin/clients` | 列出调用方（只显示 `secret_prefix`，**永不回显完整凭证**，FR-094） | **M0** |
| `POST /admin/clients/{id}/revoke` | 吊销（置 `status=revoked` + 记录 `revoked_at`/`revoke_reason`），立即生效。⚠️ **遇 `is_system=true` 返回 403** —— 内置 `system-probe` 被误吊销会让主动测活整体静默失效 | **M0** |
| `POST /admin/clients/{id}/rotate` | 轮换 = 新签发 + 旧凭证宽限期后自动吊销。⚠️ **`is_system=true` 同样 403** | M1 |
| `GET /admin/reservations?needs_review=true` | 列出待人工核对的保守结算（`unknown_billing`/`interrupted` 崩溃恢复产生，[02 §2bis](./02-data-model.md)） | M1 |
| `POST /admin/reservations/{request_id}/adjust` | 运维核对上游账单后修正实际费用。走**独立的 `adjust` 事务**（`FOR UPDATE` 锁定 reservation → 从锁定行派生 client/日期/旧值 → 按差额修正聚合），**不是 `finalize`**——`finalize` 的闸门是 `state='reserved'`，而待核对行早已是 `settled`，走它必然 0 行无效。入参只有 `new_actual_usd`/`event_key`/`operator`/`reason`，且 **`new_actual_usd < 0` 直接 400**；**client 与日期不可由调用方指定**，且**严禁直接改 `client_daily_spend`** | M1 |
| `GET/POST/DELETE /admin/data-policies` | 数据许可规则 CRUD（[02 §2ter](./02-data-model.md)，FR-093）。开关 `data_policy_enabled` 默认 `false`，走 §3 二次确认 | M4 |
| `POST /admin/data-policies/simulate` | 传 **`gateway_client_id`**（属性由服务端从该凭证行取，**不接受调用方自报**），返回允许渠道列表 + 每个被排除渠道的原因（`data_policy_denied`/`data_policy_no_match`）；与请求路径共用求值实现——**AC-14 的可执行判定入口** | M4 |

---

## 6. FR / AC 覆盖

| 契约要素 | FR / AC |
| --- | --- |
| 策略可配置 + 建议默认 | FR-115、DECISIONS 总原则 |
| 关键项二次确认 | FR-115、[02 `confirmed_twice`](./02-data-model.md) |
| 明示含义/用法/影响 | FR-115（`ParamMeta.Description/ImpactHint`） |
| 配置版本化、前后值、责任人、原因 | FR-099/104、[02 `config_params`](./02-data-model.md) |
| 数据时效可配置 | FR-116 |
| 别名策略载体管理 | FR-062/117 |
| 账本可查询 | FR-097/098 |
| 入站凭证签发/吊销/轮换（不回显明文） | FR-094、FR-120、AC-33 |
| 数据许可规则管理 + simulate | FR-093（默认关）、AC-14 |

---

## 7. 开放点

| # | 开放点 | 结论 |
| --- | --- | --- |
| 1 | 设置页 UI 一期做到什么程度 | ✅ **已定**：一期只保证 API + 二次确认 + 元数据（可 curl/极简表单）；完整 UI 后置 M4 |
| 2 | 管理令牌 vs 多用户 RBAC | ✅ **已定**：一期单管理令牌（个人/内部）；多用户 RBAC 对外前再评估 |
| 3 | 管理平面是否也要多实例一致 | ✅ **已定**：无状态，写走同一 PG（`config_params` 版本化），任一 core 实例可服务；与数据平面同源（[06](./06-deployment-and-operations.md)） |

---

_本篇补齐 FR-115 的操作面：`config_params` 表 + 本篇 API + 二次确认流程三者合起来才让"策略可配置且关键项二次确认"可落地。UI 可后置，契约先定。_
