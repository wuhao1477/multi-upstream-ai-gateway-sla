# ISSUE-005：开发阶段 P1 ＝ 多上游渠道采集与管理（最小设计版，待 PM 确认）

| 项目 | 内容 |
| --- | --- |
| 状态 | ✅ **已确认并落地（2026-07-27）** —— 11 条决定 PM 全部采纳推荐方案；已回写 [DECISIONS](../DECISIONS.md)、PRD 升 v1.5、同步 00/02/04/09/14/15 与门禁 |
| 日期 | 2026-07-27 |
| 发起 | 开发（整体交付体量过大，按负责人指示重新切分交付进度） |
| 负责人指示 | ① **充值倍率一期不做**（多数站点 1:1，少数 1:2）；② **数据库不过度设计，每阶段只做最小设计**（[ponytail](https://github.com/DietrichGebert/ponytail) 决策阶梯）；③ 缺失的必要设计要补上，**分清当期与后期**；④ **当期重点是上游采集与管理，不做网关相关部分**；⑤ **原「一期」（SLA 网关全套 31 条 AC）升为最终目标**；⑥ **第一期开发进度（P1）= 仅实现多上游渠道的采集与管理** |
| 本版与上一版的差别 | 上一版提 4 张新表 + 3 组 ALTER + 8 条 FR；按指示②④重审后 → **3 张新表 + 1 组 ALTER + 7 条 FR**，砍掉 1 张表、11 个字段、2 条 FR、2 条 AC（逐项理由见 §3） |
| 使用方式 | ✅ 已确认完毕。本文档转为**裁决记录**：§0 是决定速览，§3 是数据库最小设计的逐项理由，§6 是后续阶段任务台账 |

---

## 0. 决定速览（PM 只填最后一列）

| # | 事项 | 推荐方案 | 决定 |
| --- | --- | --- | --- |
| P0 | **术语分层**（必答） | 原「一期」31 条 AC **升为最终目标**（需求范围，不代表交付顺序）；交付进度另立 **P1~P4**。现有文档 249 处「一期」/82 处「二期」**一字不动**，只在 PRD §2.1 加一张映射表。理由见 §1 | ✅ 采纳。落地为 [PRD §2.1.0](../PRD.md) + [00 §3](../dev/00-overview-and-milestones.md) 两张表 |
| P1 | P1 交付边界 | **完全不含请求转发**：不碰 `/v1/*`、不碰账本、不碰调度、不碰 SLA。只做"把上游资产采准、可管、可查" | ✅ 采纳 |
| D1 | 充值倍率 | **P1 不做**（照指示①）。代价签收：1:2 充值渠道成本会被高估 2 倍——但 P1 无成本排序，**无消费者**，故不是缺口。登记为 P2 任务（§6） | ✅ 采纳 |
| D2 | 分组实体 | 新增 `channel_groups`（**6 列**，砍掉高峰倍率等 6 字段）+ `group_models`（3 列） | ✅ 采纳 |
| D3 | Key 用量 | `upstream_keys` 补 6 列存**当前值**；历史时序**复用已有 `collector_snapshots`**（`scope_type='key'`），不建新表 | ✅ 采纳 |
| D4 | 渠道模型目录 | 新增 `channel_model_catalog`（**6 列**，砍掉能力位与启用关联） | ✅ 采纳 |
| D5 | Key 级限流 | P1 **只采集存储**（`upstream_keys.rpm_limit`/`concurrency_limit` 两列），不做派生、不做判闸——判闸属容量保留，是网关的事 | ✅ 采纳 |
| D6 | 「实时更新」语义 | 上游无 webhook → 不做推送式实时。`POST /admin/channels/{id}/sync` 手动立即刷新 + 周期采集（FR-116），界面显示 `synced_at` 与陈旧标记 | ✅ 采纳 |
| D7 | 密钥管理边界 | P1 做**上游 Key** 的 CRUD＋轮换＋停用＋脱敏展示。入站凭证 `gateway_clients` 属网关，**P1 不做** | ✅ 采纳 |
| N1 | 新增 FR | FR-122~FR-128（7 条，§4） | ✅ 采纳 |
| N2 | 新增 AC | AC-37~AC-40（4 条，§4） | ✅ 采纳 |

---

## 1. 术语分层（P0，必须先答）

### 问题：两个不同维度用了同一个词

- PRD 现有的「一期 / 二期」是**需求范围**的划分：一期＝SLA 网关全套（31 AC），二期＝订阅制/多等级/外部告警完整渠道（5 AC）。
- 负责人说的「第一期开发」是**交付进度**：先只做采集与管理。

两者正交。硬把 249 处「一期」改成别的词有三个代价：**DECISIONS.md 是 PM 逐项确认的历史记录**（改它等于篡改确认结果）；很多句子是引用当时原话（如「一期明文存储，不加密不强制轮换」）；「一期 vs 二期」的需求范围划分本身**依然有效**（订阅制确实推迟）。

### 推荐：加一层术语，不动存量

| 层 | 名称 | 含义 | 现状 |
| --- | --- | --- | --- |
| **需求范围** | **最终目标** | 产品要做到的完整形态。＝原「一期」31 AC + 原「二期」5 AC + P1 新增 4 AC | 现有文档的「一期」标签**保持不动**，语义读作"最终目标内的必做项" |
| **交付进度** | **P1 / P2 / P3 / P4** | 分几批交付、当前做哪批 | **新增**，只写在 PRD §2.1 与 00 里程碑 |

### 交付进度表（新增）

| 阶段 | 内容 | AC | 现在的位置 |
| --- | --- | --- | --- |
| **M0** | 骨架（compose / 迁移 / `/admin` / CI），全阶段公共前置 | AC-27、AC-33-M0 | 已冻结，不动 |
| **P1** | **多上游渠道采集与管理**（本 ISSUE） | AC-37~40（新增 4 条） | ⬅ **现在做这个** |
| **P2** | 网关核心：字节透传 / 内容感知 TTFT / 首字前接管 / 逐 Attempt 账本 / 入站鉴权与配额 | AC-01/16/26/30/31/32/35、AC-06/07/12/15/25、AC-33-M1 | 原 M1+M2，内容一字不改 |
| **P3** | 调度与经营闭环：候选过滤排序 / canary / 主动测活 / 容量保留 / 告警 / 压测验收 | AC-02/03/04/05/17/19/28/29、AC-08/09/10/11/13/14/18/34/36 | 原 M3+M4，内容一字不改 |
| **P4** | 订阅制适配、多 SLA 等级、外部告警完整渠道 | AC-20~24 | 原「二期」，不动 |

- **最终目标 = P1 + P2 + P3 + P4**，合计 **40 条 AC**（36 现有 + 4 新增）。
- 原 M1~M4 的**里程碑内容、退出标准、验收判定全部一字不改**，只是被归入 P2/P3——避免返工。
- **落地动作只有两处**：PRD §2.1 加上表 + 一句"文中『一期』指需求范围（最终目标内），交付顺序见本表"；00 里程碑表加阶段列。其余 331 处期别标签不动。

---

## 2. P1 做什么（最小集）

**做**：三家族采集适配器（`Detect`/`Authenticate`/`FetchAccount`/`FetchKeys`/`FetchGroups`/`FetchPricing`/`FetchModelCatalog`）· 渠道/账号/Key 管理 CRUD · 分组与分组可用模型 · Key 用量同步 · 渠道模型目录 · 价格版本（不可覆盖，为 P2 铺路）· 手动刷新 · 资产总览

**不做**（全部属 P2 网关）：请求转发 · 账本 · 调度与候选过滤 · SLA/TTFT/接管 · canary/测活 · 入站凭证与配额 · 余额信号自适应识别（依赖真实请求失败信号，P1 无请求路径）· 容量保留判闸

**依赖**：只依赖 M0（compose、PG 迁移、`/admin` 骨架、`ADMIN_TOKEN`、CI 门禁）。交付后立即可用——能看清 20 个渠道有什么、值多少钱、Key 还剩多少。

---

## 3. 数据库最小设计（3 张新表 + 1 组 ALTER）

> 按 ponytail 决策阶梯逐项过：**这张表/这个列，P1 有没有消费者？** 没有 → 不建。
> 下面每砍一项都写了理由，便于 PM 判断我是否砍错。

### 3.1 新增 `channel_groups`（6 列）

**为什么必须有**：44 张表里**没有 group 实体**。分组现在只是 `multiplier_versions.group_multiplier` 一个数字——"这把 Key 属于哪个分组""分组能用哪些模型""分组倍率是多少"全部无处安放。[04](../dev/04-collector-adapter.md) 的 `Group` Go 结构采回来后无表可落，只能塞 JSONB，查不了也用不了。

```sql
CREATE TABLE channel_groups (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  channel_id    BIGINT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  group_ref     TEXT NOT NULL,               -- 上游分组标识（NewAPI group / Sub2API group_id）
  rate_multiplier NUMERIC(12,6),             -- 分组倍率
  data_source   TEXT NOT NULL CHECK (data_source IN ('auto_collect','manual')),
  fetched_at    TIMESTAMPTZ NOT NULL,
  UNIQUE (channel_id, group_ref)
);
```

**砍掉的 6 个字段与理由**：

| 砍掉 | 理由 |
| --- | --- |
| `display_name` | `group_ref` 本身就是可读的（如 `default`/`vip`），P1 无二次命名需求 |
| `peak_enabled`/`peak_start`/`peak_end`/`peak_rate_multiplier` | 高峰倍率只有 Sub2API 系有，且**消费者是成本排序**（P2）。P1 采回来没人读 → 需要时再加列（`ALTER ADD COLUMN` 无损） |
| `is_exclusive`/`platform` | 同上，调度用字段，P1 无消费者 |
| `valid_until` | 陈旧性由 `fetched_at` + 配置阈值查询期计算即可，与 `collector_snapshots` 同一做法（那里已刻意不存 `is_stale`） |

### 3.2 新增 `group_models`（3 列）

**为什么必须有**：「Key 分组对应可获取的模型」是明确诉求，且**不能借道 `channel_models`**——后者要求 `model_id` 外键指向已登记的 `models`，而分组可用模型是上游原始名，不需要（也不该）先登记。

```sql
CREATE TABLE group_models (
  channel_group_id BIGINT NOT NULL REFERENCES channel_groups(id) ON DELETE CASCADE,
  model_name    TEXT NOT NULL,               -- 上游原始名，不要求已在 models 表登记
  fetched_at    TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (channel_group_id, model_name)
);
```

**砍掉 `available BOOLEAN`**：采到就是可用，采不到就删行。加一个恒为 true 的列没有信息量。

### 3.3 新增 `channel_model_catalog`（6 列）

**为什么必须有**：「渠道所有可用模型列表」现在只能进 `models` 表，而它要求 `max_input_tokens`/`max_output_tokens` **必填**（[02 §2bis](../dev/02-data-model.md) 预留上界算法的硬前置，缺了该模型全部 binding 不进候选）。一个中转站 200~300 个模型 × 20 渠道 = 几千行手填 token 上界，不可行。

```sql
CREATE TABLE channel_model_catalog (
  channel_id    BIGINT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  model_name    TEXT NOT NULL,               -- 上游原始名
  input_price   nonneg_usd,                  -- 采到的价格，供选型参考
  output_price  nonneg_usd,
  first_seen_at TIMESTAMPTZ NOT NULL,
  last_seen_at  TIMESTAMPTZ NOT NULL,        -- 停止更新 = 上游下架了它
  PRIMARY KEY (channel_id, model_name)
);
```

**砍掉的 5 个字段与理由**：

| 砍掉 | 理由 |
| --- | --- |
| `cache_price`/`billing_unit` | 价格进 `price_versions` 才是权威（不可覆盖版本，FR-012）；目录里两列只为"看一眼贵不贵"，缓存价与计费单位 P1 没人看 |
| `supports_streaming`/`supports_tools`/`raw_capabilities` | 能力位的消费者是 selector 候选过滤（P2）。且**站点自报的能力位不可信**——P2 本来就要用 `Probe()` 实测（[03 §8](../dev/03-upstream-layer.md) 已定），采自报值等于存一份将被推翻的数据 |
| `enabled_model_id` | 上一版设计的"目录 ↔ 启用态"双向关联。**P1 不需要**：P1 不做路由，没有"启用"这个动作。P2 要启用时用 `(channel_id, model_name)` 去 `models.canonical_name` 匹配即可，不必现在就建外键 |

> ⚠️ 砍掉 `enabled_model_id` 连带砍掉了上一版的 `POST /admin/channel-models/enable` 端点与 AC-40。**这不是遗漏**：启用模型是为了让它能被路由，而 P1 没有路由。

### 3.4 `upstream_keys` 补 6 列

**为什么必须有**：`upstream_keys` 现在**没有任何余额/用量列**。[04 §4](../dev/04-collector-adapter.md) 声称 `FetchKeys` 写入 `upstream_keys` + `collector_snapshots`，实际前者无列可写——"Key 还剩多少额度"这个最基本的问题查不出来。

```sql
ALTER TABLE upstream_keys
  ADD COLUMN channel_group_id BIGINT REFERENCES channel_groups(id),  -- Key 归属分组
  ADD COLUMN remain_quota_usd nonneg_usd,     -- 剩余额度（归一美元）
  ADD COLUMN used_quota_usd   nonneg_usd,     -- 已用额度
  ADD COLUMN rpm_limit        INTEGER,        -- 上游 Key 级 RPM（采集或登记；P1 只存不判）
  ADD COLUMN concurrency_limit INTEGER,       -- 上游 Key 级并发（同上）
  ADD COLUMN quota_synced_at  TIMESTAMPTZ;    -- 最近同步时刻（陈旧判定）
```

**用量历史不建新表**：上一版提的 `key_usage_snapshots` 已砍——已有 `collector_snapshots` 就是干这个的（`scope_type='key'`、`payload` JSONB、带 `data_source`/`fetched_at`/`valid_until` 三元组、已有保留策略与索引）。查历史用量走它，`upstream_keys` 只存当前值供列表展示。

**Key 归属分组用单列而非关联表**：P1 的 20 个渠道均为中转站，Key 建好后分组基本固定。多对多要传导到 `bindings` 唯一性定义与健康统计，代价不对等；真需要切组时改这一列即可。

### 3.5 P1 不建 / 不加的（明确登记，避免以后当成遗漏）

| 项 | 为什么 P1 不做 | 何时做 |
| --- | --- | --- |
| **充值倍率**（`topup_rate`） | 指示①：多数站点 1:1。且它唯一的消费者是**成本排序**，P1 没有成本排序 → 采了也没人读 | P2（网关成本优选上线时），见 §6 |
| `key_usage_snapshots` | `collector_snapshots` 已覆盖 | — |
| 限流三层 MIN 派生 | 消费者是容量保留判闸（P2） | P2 |
| `capacity_claims` 按 Key 归集修正 | 它是**P2**的表（容量保留），P1 不碰 | P2，见 §6 |
| 高峰倍率 / 独占标记 / 平台归属 / 自报能力位 | 消费者都在 P2 调度 | P2 按需 `ALTER ADD COLUMN` |

---

## 4. 新增 FR / AC

### FR（FR-122~128，随 PRD 升版）

| 编号 | 优先级 | 需求 |
| --- | --- | --- |
| FR-122 | P0 | 上游 Key 全生命周期管理：新增、编辑、停用、轮换、删除。明文仅在登记时接收，展示与日志一律脱敏（承 FR-094/113）。一个渠道可挂多个账号、一个账号可挂多个 Key。 |
| FR-123 | P0 | 采集并维护**渠道分组**及其倍率，并记录每把 Key 所属分组。 |
| FR-124 | P0 | 采集并维护**每个分组可获取的模型清单**，支持按分组查询"这把 Key 能用哪些模型"。 |
| FR-125 | P0 | 同步并展示每把 Key 在上游的**使用情况**：剩余额度、已用额度、同步时刻；历史用量落采集快照以支持消耗速度估算。 |
| FR-126 | P0 | 采集并维护**渠道全部可用模型目录**（含采到的价格）；模型被上游下架时可识别并告警。 |
| FR-127 | P0 | 采集并存储上游施加的 **Key 级 RPM 与并发上限**（P1 只做登记与展示，执行属 P2 容量保留）。 |
| FR-128 | P0 | 支持**按渠道手动触发立即刷新**全部上游元数据，与 FR-116 周期采集并存；界面须显示每项数据的同步时刻与陈旧标记。 |

> 上一版的 FR-129「资产总览」已并入 FR-128 的展示要求（一个 `GET` 端点，不值得单列一条 P1 需求）。

### AC（AC-37~40，归属 P1）

| 编号 | 场景 | 环境 | 判定方法 |
| --- | --- | --- | --- |
| AC-37 | 一个渠道挂 2 账号、每账号 2 把 Key、分属不同分组 | FIXTURE | `GET /admin/channels/{id}/inventory` 返回 2 账号 / 4 Key / 各自分组与倍率；4 把 Key 明文**均不回显**（只见 `secret_prefix`）；库中 4 行 `upstream_keys.channel_group_id` 非空 |
| AC-38 | 三家族站点各触发一次手动同步（FR-128） | **REAL** | `POST /admin/channels/{id}/sync` 返回逐项结果与耗时；`channel_groups`/`group_models`/`channel_model_catalog` 三表与 `upstream_keys` 用量列均更新；不支持的项返回 `unsupported` 而非留空（承 AC-28 口径） |
| AC-39 | 渠道目录含 200+ 模型 | FIXTURE | `channel_model_catalog` 200+ 行**无需任何 token 上界**即可入库；`GET /admin/channels/{id}/catalog` 可分页查询并按价格排序；`models` 表**不因此产生任何行** |
| AC-40 | 上游下架某模型 | FIXTURE | 连续 N 轮采集后该行 `last_seen_at` 停止更新 → 产生 P3 `alert_events(category='model_capability')`（复用已有枚举，不新增） |

> 上一版的 AC-38（充值倍率成本）随 D1 砍掉；AC-40（目录→启用）随 §3.3 砍掉；AC-42（限流 MIN 与按 Key 归集）随 D5 移 P2。

---

## 5. 管理端点（P1 交付，补入 [09](../dev/09-admin-api.md)）

| 端点 | 作用 |
| --- | --- |
| `GET/POST/PATCH /admin/channels` | 渠道 CRUD（现只能经 `/admin/bindings` 间接建） |
| `GET /admin/channels/{id}/inventory` | 资产总览：账号/Key/分组/模型数、额度合计、同步时刻、异常计数（FR-128 展示面） |
| `POST /admin/channels/{id}/sync` | 手动立即刷新，返回逐项结果与 `unsupported` 标记（FR-128） |
| `GET/POST/PATCH /admin/accounts` | 账号 CRUD |
| `GET/POST/PATCH /admin/keys` | Key CRUD（FR-122）；列表**只回 `secret_prefix`** |
| `POST /admin/keys/{id}/rotate`、`/disable` | Key 轮换与停用 |
| `GET /admin/keys/{id}/usage?from=&to=` | Key 用量历史（读 `collector_snapshots`，FR-125） |
| `GET /admin/channel-groups?channel_id=` | 分组列表含倍率（FR-123） |
| `GET /admin/channel-groups/{id}/models` | 分组可用模型（FR-124） |
| `GET /admin/channels/{id}/catalog` | 渠道模型目录，分页 + 按价格排序（FR-126） |

> 鉴权边界不变：全挂 `/admin/*`，走 `ADMIN_TOKEN` + Caddy 不代理（[09 §1](../dev/09-admin-api.md)）。P1 **不新增任何 `/v1/*` 端点**。

---

## 6. 后续阶段任务登记（P1 刻意不做，避免以后当遗漏）

| # | 任务 | 触发时机 |
| --- | --- | --- |
| T-1 | **充值倍率**：账号级 `topup_rate`（默认 1.0），成本排序改用「额度 ÷ 充值倍率」的现金口径 | 成本优选上线时（P3，成本排序属调度）。⚠️ 不做则 1:2 充值渠道成本被高估 2 倍、永远选不上——与"降真实成本"的目标直接冲突，**P2 必须做** |
| T-2 | **`capacity_claims` 按 Key 归集**（现按 `binding_id`）。同一把 Key 下 N 个 binding 共享上游配额，各自判闸会放行 N 倍——**容量保留在多模型渠道上实际失效**。这是既有缺陷，非本 ISSUE 引入 | 容量保留上线时（P3） |
| T-3 | 限流三层 MIN 派生（binding / Key / 分组取最紧） | 同 T-2 |
| T-4 | 高峰倍率、独占标记、平台归属、`Probe()` 实测能力位 | P3 调度需要时 `ALTER ADD COLUMN` |
| T-5 | 模型"启用"动作（目录 → `models`+`channel_models`，token 上界必填） | P2 路由上线时（候选过滤需要已登记模型） |
| T-6 | 余额信号自适应识别（FR-027/AC-29） | P3（依赖真实请求失败信号，属调度与经营闭环） |

---

## 7. 文档影响面（确认后执行）

> **原则：存量期别标签一字不动**（§1）。下表只有"加"，没有"改"。

| 文档 | 改动 | 量级 |
| --- | --- | --- |
| [PRD](../PRD.md) | 升 **v1.5**：§2.1 **加**交付进度表（§1）+ 一句术语说明；**加** FR-122~128、AC-37~40；术语表加"分组""模型目录" | 加 3 段 |
| [02 数据模型](../dev/02-data-model.md) | **加** 3 表 + `upstream_keys` 6 列；每处标 `P1` | 加 4 段 |
| [04 采集器](../dev/04-collector-adapter.md) | **加** `FetchModelCatalog`；§4 写入映射表**改** 2 行（现指向"无列可写"的 `upstream_keys`，是既有错误）；`Group` 结构按 §3.1 精简 | 加 1 段 + 改 2 行 |
| [09 管理 API](../dev/09-admin-api.md) | **加** §5 的 10 个端点，标 `P1` | 加 10 行 |
| [00 里程碑](../dev/00-overview-and-milestones.md) | 里程碑表**加**"阶段"列（M0→M0、M1/M2→P2、M3/M4→P3）+ **加** P1 一行；M3 行**改**：去掉已在 P1 交付的采集项 | 加 1 列 1 行 + 改 1 行 |
| [14 验收矩阵](../dev/14-acceptance-matrix.md) | **加** P1 段落 4 条 AC；§1 总览**加**阶段列；合计 36 → **40** | 加 1 段 |
| [15 范围](../dev/15-scope-and-preflight.md) | **改** T6（价格覆盖率验证提前到 P1 完成） | 改 1 行 |
| `verify/check_docs.py` | AC 计数断言 31/5/36 → **35/5/40**（P1 新增 4 条计入"一期"侧，因它们属最终目标内必做项） | 改 1 处 |
| **不动** | DECISIONS.md、ISSUE-001~004、01/03/05/06/10/11/12/13 | 0 |

---

## 8. 处理顺序

1. **先答 P0（术语分层）**——它决定所有文档怎么标。若不同意"存量不动"，请说明期望的改法（全局重编号需改 331 处，含 DECISIONS 的历史确认记录）。
2. D1~D7 + N1/N2 逐条 ✅。
3. 确认后：PRD 升版 → 02 建表（3 表 + 1 ALTER）→ 04 补契约 → 09 补端点 → 14 补 AC → 跑门禁（DDL 真跑 + 12 类检查 + AC 计数）。
4. §6 的 T-1/T-2 请确认已登记在案——它们是 P1 刻意不做、后续阶段必须做的，尤其 **T-2 是既有缺陷**（容量保留在多模型渠道上目前实际失效）。
