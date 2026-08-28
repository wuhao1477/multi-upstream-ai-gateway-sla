# 02 数据模型（自研 SLA 核心 · PostgreSQL 单库 · v0.1 草案）

| 项目 | 内容 |
| --- | --- |
| 状态 | ✅ **v1.0 基线（2026-07-26 冻结）** —— 经 42 轮对抗性审查（含 5 轮开发视角）+ PM 开工前裁决；变更须走版本记录；DDL 已在 postgres:16 实测通过（`verify/ddl-check.sh`） |
| 日期 | 2026-07-23 |
| 栈 | Go（pgx + sqlc）/ PostgreSQL 单库（一期不引 Redis）/ 单机 Docker Compose / **无外部网关**（[11 转向](./11-decision-full-selfbuilt.md)） |
| 输入 | [PRD v1.5](../PRD.md)（FR-001~128、**AC-01~40**、§11 默认策略、§13 参数）、[DECISIONS](../DECISIONS.md)（16 参数 + 5 新需求 + **ISSUE-004 开工前裁决 20 条** + **ISSUE-005 交付切分 11 条**）、[ISSUE-001 运行时结论](../issues/ISSUE-001-tech-assumption-verification.md)（六假设；其 AxonHub schema 适配表已随 [11 转向](./11-decision-full-selfbuilt.md) 转为历史）、[ISSUE-002 采集适配器](../issues/ISSUE-002-collector-adapter-design.md)（三家族、sub2api ent schema、凭证生命周期）、[ISSUE-002 探测实测](../issues/ISSUE-002-probe-results.md)（四站真实字段）、[00 总览](./00-overview-and-milestones.md)（硬约束 11 条）、[01 架构](./01-architecture.md)（sla-core 模块、自研上游直连） |
| 覆盖范围 | 本篇定义**自研 SLA 核心的 PG 库**结构。转向自研后（[11](./11-decision-full-selfbuilt.md)），本库是**账本唯一真相源**，无外部网关账本需对账 |

---

## 0. 建模总则与约定

> 这些约定贯穿全库，逐条对应一条硬约束，先立规再建表。

### 0.1 硬约束在数据层的落地

| 硬约束 | 数据层做法 | 来源 |
| --- | --- | --- |
| 账本元数据 ≥180 天且可查询 | `requests`/`attempts`/`attempt_usage` 按 `月` RANGE 分区，保留窗口靠 `DETACH+DROP` 整分区滑动（见 §9） | FR-112 |
| 一期不存正文/上下文/请求头/请求体 | **全库无 `body`/`messages`/`prompt_text`/`headers` 列**；只存 token 计数、延迟、状态、费用等元数据；采集侧只存额度/价格数值，不存上游返回正文 | FR-112 |
| 多实例共享（sla-core ×2 无本地状态） | 所有写入走同一 PG；账本主键用 **UUIDv7**（时间有序、无需跨实例协调序列），配置/注册表用 `BIGINT IDENTITY` | FR-110 |
| 决策 P99≤50ms | 同步决策路径**只读内存快照**，不查 PG；本库承担写路径与后台快照重建，不在关键路径上 | FR-110、01-架构 §5 |
| 上游 Key 一期明文 | `upstream_keys.secret` / `collector_credentials.*` 明文列，仅在应用层脱敏展示（FR-094） | FR-113 |
| TTFT 不采信网关字段 | attempt 同时存**自算内容感知 TTFT** 与**网关上报值（仅存证）**两列，语义上永不混用 | AC-31、假设 3/6 |
| 取消按 errorMessage 归并 | attempt 存**网关原始 status** 与**归并后的 `cancel_reason`** 两列 | AC-30、假设 2 |
| 记账不假设「一次调用=一次上游用量」 | attempt 存 `upstream_call_count`（隐藏重试补算）与外部调用恒为 1 attempt 的关系 | FR-119、假设 6 |

### 0.2 通用类型与命名

```sql
-- 统一美元金额域：一期各币种 1:1（FR-018），币种仅名称保留在各表 currency 列
-- 精度覆盖到 1e-10（实测最小成本样本 1.3e-5，见 ISSUE-001 假设 4）
-- 通用金额（**可正可负**）：用于差额类派生值
CREATE DOMAIN usd_amount AS NUMERIC(20,10);
-- 非负金额：价格、费用、余额、配额、预留/结算等**语义上不可能为负**的列一律用它
CREATE DOMAIN nonneg_usd AS NUMERIC(20,10) CHECK (VALUE >= 0);
-- ⚠️ 第 11 轮曾直接给 usd_amount 加非负约束 —— **是回归**（第 12 轮 [high]）：
--    该域已用于 attempt_usage.cost_variance（= 实际−预估，实际更低时必然为负）与
--    balance_signals.conservative_floor（= 最近余额−已知消耗，可低于零）。
--    合法数据会撞约束、整笔用量或余额信号无法入库。故拆成两个域，按语义选用。
-- 非负的意义（`adjust` 是唯一人工修正入口）：若允许负值，一次 settled_usd += (new − old)
--    就能把已结算消费改成负数并清掉 needs_manual_review，长期放宽日配额而数据库不拒绝。

-- 采集侧多种额度单位（quota整数 / USD浮点 / micros）由适配器入库前归一为 usd_amount（ISSUE-002 §6.4）
-- 所有时间戳统一 TIMESTAMPTZ（UTC 存储）
-- 所有「数据来源+更新时间+有效期」三元组统一列名：data_source / fetched_at / valid_until
```

- 账本表主键：`id UUID DEFAULT ...`（应用层生成 **UUIDv7**，保证时间有序 + 多实例无冲突）。
- 注册/配置表主键：`id BIGINT GENERATED ALWAYS AS IDENTITY`。
- 枚举一律用 `TEXT + CHECK` 约束（便于扩枚举时不用 `ALTER TYPE`，迁移零锁表）。
- `data_source` 取值：`auto_collect`（适配器自动采）/ `manual`（人工录入，7 天有效，FR-011）/ `upstream`（上游回传）/ `derived`（自算）。

---

## 1. 资源注册域（账本与台账的外键基座）

> 支撑 FR-001~005 的身份登记；本篇按需给出，重点深度在后面 6 个域。「路由资源」= 渠道+账号+Key+模型+地区+缓存作用域（FR-002、术语§3）。

### 1.1 渠道 / 真实上游 / 账号 / Key / 模型

```sql
-- 真实上游 / 供应商（FR-002/044）：多个渠道可共享同一官方上游 → 故障域根
-- ⚠️ 必须先于 channels 创建（channels 有外键指向它）
CREATE TABLE upstream_providers (
  id      BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name    TEXT NOT NULL,          -- 如 openai/anthropic 官方供应商
  note    TEXT
);

-- 渠道（FR-001/002/004）：可持续新增/停用/恢复
CREATE TABLE channels (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name            TEXT NOT NULL,
  site_family     TEXT NOT NULL CHECK (site_family IN ('newapi','sub2api','asxs','unknown')), -- ISSUE-002 §1 三家族
  base_url        TEXT NOT NULL,
  upstream_provider_id BIGINT REFERENCES upstream_providers(id),   -- 真实上游（故障域根，FR-044）
  status          TEXT NOT NULL DEFAULT 'enabled' CHECK (status IN ('enabled','disabled')), -- FR-004
  disabled_reason TEXT,           -- FR-095 人工停用原因
  disabled_until  TIMESTAMPTZ,    -- FR-095 有效期
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 账号（FR-002/003/020/022）：一账号可共享多 Key、共享余额
CREATE TABLE upstream_accounts (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  channel_id    BIGINT NOT NULL REFERENCES channels(id),
  external_user_id TEXT,          -- NewAPI 数字用户ID（New-API-User 头必需，ISSUE-002 §3.1）
  balance_group_key TEXT,         -- 共享余额分组键：同 key 的多账号/多Key 只算一次余额（FR-022/AC-04）
  -- 人工停用（五层停用开关之一）
  status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
  disabled_reason TEXT,
  disabled_until  TIMESTAMPTZ,    -- NULL = 无限期停用，须人工恢复
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 上游 Key（FR-002/003/031）：独立倍率/额度/模型权限；一期明文（FR-113）
CREATE TABLE upstream_keys (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  account_id    BIGINT NOT NULL REFERENCES upstream_accounts(id),
  secret        TEXT NOT NULL,    -- 明文（FR-113）；展示层脱敏（FR-094）
  key_multiplier NUMERIC(12,6),   -- Key 级倍率（FR-003）
  expired_time  TIMESTAMPTZ,      -- NewAPI /api/token.expired_time（ISSUE-002 §3.1）
  unlimited_quota BOOLEAN DEFAULT false,
  model_limits  JSONB,            -- NewAPI model_limits（元数据，非正文）
  status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','revoked','expired','insufficient_perm')), -- FR-031
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 模型能力（FR-005/006）：逐资源记录真实支持能力
CREATE TABLE models (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  canonical_name TEXT NOT NULL,   -- 真实上游模型名（区别于对外别名）
  -- ⚠️ 曾有一列 `max_context`，第 40 轮**删除**：它与下面的 `max_input_tokens` 语义重叠
  --    （注释里 max_input_tokens 就写着「通常=上下文窗口」），却没有任何规则读它。
  --    两个含义相近的上限列并存必然导致"预留用 A、能力判定用 B"的分裂。留一个。
  -- ── 费用预留上界所需（§2bis 预估算法）。**两列缺一，该模型的 binding 不得进候选** ──
  -- 曾把输出上限写成 channel_models.max_output_tokens —— 那一列**根本不存在**（第 9 轮 critical）。
  max_input_tokens  INTEGER,      -- 协议级最大可计费输入（通常=上下文窗口）；上游物理上不可能计费超过它
  max_output_tokens INTEGER,      -- 最大可生成输出 token 数
  -- 写入载体：POST/PATCH /admin/models（[09 §5](./09-admin-api.md)），接口层强制 NOT NULL AND > 0；
  -- 列本身可空只为兼容历史行，**业务上等价于必填**（为空即该模型全部 binding 不进候选）
  supports_streaming BOOLEAN,
  supports_tools     BOOLEAN,
  supports_structured_output BOOLEAN,
  supports_multimodal BOOLEAN,
  supports_reasoning  BOOLEAN,
  UNIQUE (canonical_name)
);

-- 渠道×模型×**协议**能力矩阵（FR-005/006）
-- ⚠️ 必须按协议细分：实测发现同一站点 gpt-5.5 的 Responses 返 200 而 CC 返 503
--    （[07 §3bis](./07-axonhub-runtime-probes.md)、[13 §3](./13-research-reassessment.md)）。
--    若只存 (channel, model, enabled)，CC 请求仍会被路由到 Responses-only 渠道 → 必然失败。
CREATE TABLE channel_models (
  channel_id  BIGINT NOT NULL REFERENCES channels(id),
  model_id    BIGINT NOT NULL REFERENCES models(id),
  protocol    TEXT NOT NULL CHECK (protocol IN ('chat_completions','responses')),
  enabled     BOOLEAN NOT NULL DEFAULT true,  -- 人工开关（FR-004）
  -- ── 该渠道对这个模型的**上游称呼**（第 40 轮新增）──
  -- 20 个中转站对同一模型叫法不同是常态（`gpt-5.5` / `gpt-5.5-0930` / `openai/gpt-5.5`…）。
  -- 此前只有全局唯一的 models.canonical_name，**无处存放分渠道差异** → 发给上游必然模型名错。
  -- NULL = 该渠道就用 models.canonical_name（多数情况）。
  upstream_model_name TEXT,
  -- ── Probe() 探测结果（[03 §8](./03-upstream-layer.md)）──
  support     TEXT NOT NULL DEFAULT 'unknown'
                CHECK (support IN ('supported','unsupported','unknown')),
  supports_streaming BOOLEAN,                 -- 该协议下是否支持流式
  supports_tools     BOOLEAN,                 -- 是否支持工具调用
  probed_at   TIMESTAMPTZ,                    -- 最近探测时间；过期需重探
  probe_failure_reason TEXT,                  -- 如 "503 no available channel"，供排障
  PRIMARY KEY (channel_id, model_id, protocol)
);

-- selector 候选过滤按**请求协议**查此表：只选 support='supported' 且 enabled 的行
-- （[05 §1.1](./05-scheduling-and-operations.md) 序 2 模型能力层）
CREATE INDEX idx_chmodel_usable ON channel_models(model_id, protocol)
  WHERE enabled AND support = 'supported';
```

### 1.2 缓存作用域 / 故障域 / 绑定（路由资源）

```sql
-- 缓存作用域（FR-055/术语§3）：连续会话优先复用可延续缓存的资源
CREATE TABLE cache_scopes (
  id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  channel_id  BIGINT NOT NULL REFERENCES channels(id),
  account_id  BIGINT REFERENCES upstream_accounts(id),
  key_id      BIGINT REFERENCES upstream_keys(id),
  model_id    BIGINT REFERENCES models(id),
  scope_label TEXT NOT NULL      -- 缓存绑定的渠道/账号/Key/模型组合标识
);

-- 故障域（FR-044/045）：一个资源可属多个故障域（官方供应商/代理层/账号/地区/网络）
CREATE TABLE fault_domains (
  id     BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  kind   TEXT NOT NULL CHECK (kind IN ('provider','proxy','account','region','network')),
  label  TEXT NOT NULL,
  -- 域级停用：selector 排除该域下**全部** binding（一次操作隔离整个故障域）
  disabled_until  TIMESTAMPTZ,
  disabled_reason TEXT,
  UNIQUE (kind, label)
);

-- 绑定 / 路由资源（FR-002/109）：账本每条 attempt 指向的最小可评估对象
-- = 渠道 + 账号 + Key + 模型 + 地区 + 缓存作用域；binding 也承载 attempt 记的 (渠道+key+url)
CREATE TABLE bindings (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  channel_id    BIGINT NOT NULL REFERENCES channels(id),
  account_id    BIGINT NOT NULL REFERENCES upstream_accounts(id),
  key_id        BIGINT NOT NULL REFERENCES upstream_keys(id),
  model_id      BIGINT NOT NULL REFERENCES models(id),
  region        TEXT,
  cache_scope_id BIGINT REFERENCES cache_scopes(id),
  -- 实际上游 URL（attempt 的 binding 三要素之一：渠道+key+url）；executor 据此发请求，
  -- 装配进 [03 §2](./03-upstream-layer.md) 的 `Binding.BaseURL`
  effective_url TEXT NOT NULL,
  -- 人工停用（selector 过滤序 4 前置判据）
  enabled       BOOLEAN NOT NULL DEFAULT true,
  -- 容量登记（B9）：保留策略的基数来源。**两列皆空 = 该渠道不启用任何容量保留**
  rpm_limit     INTEGER,                        -- 该 binding 的每分钟请求上限（登记或采集器回填）
  concurrency_limit INTEGER,                    -- 并发上限
  capacity_source TEXT CHECK (capacity_source IN ('manual','collector')),   -- 容量数据来源
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- ⚠️ 必须 NULLS NOT DISTINCT（第 19 轮 [high]）：PostgreSQL 普通 UNIQUE 允许多行 NULL，
  --    region/cache_scope_id 可空 → 「无地区、无缓存作用域」的同一 binding 可被重复创建，
  --    路由资源身份分裂、健康统计被拆成两份。
  UNIQUE NULLS NOT DISTINCT (channel_id, account_id, key_id, model_id, region, cache_scope_id)
);

-- 资源×故障域（FR-044）多对多
CREATE TABLE binding_fault_domains (
  binding_id      BIGINT NOT NULL REFERENCES bindings(id),
  fault_domain_id BIGINT NOT NULL REFERENCES fault_domains(id),
  PRIMARY KEY (binding_id, fault_domain_id)
);
```

**索引**

```sql
CREATE INDEX idx_channels_family      ON channels(site_family) WHERE status='enabled';
CREATE INDEX idx_accounts_balancegrp  ON upstream_accounts(balance_group_key); -- 共享余额只算一次（FR-022）
CREATE INDEX idx_keys_account_status  ON upstream_keys(account_id, status);
CREATE INDEX idx_bindings_channel     ON bindings(channel_id);
```

**服务 FR/AC**：FR-001~006、FR-022、FR-031、FR-044/045、FR-055、FR-095、FR-113；AC-01、AC-04、AC-05、AC-11。

---

### 1.3 上游分组与模型目录（**交付阶段 P1**，FR-123~127）

> **为什么这三张表必须存在**（[ISSUE-005 §3](../issues/ISSUE-005-phase1-upstream-inventory.md)）：
> ① 全库此前**没有 group 实体** —— 分组只是 `multiplier_versions.group_multiplier` 一个数字，而 [04](./04-collector-adapter.md) 的 `Group` 结构（倍率/限流/可用模型）采回来无表可落，只能塞 `collector_snapshots.payload`，查不了也喂不进任何规则。
> ② `upstream_keys` **没有任何余额/用量列**，而 [04 §4](./04-collector-adapter.md) 声称 `FetchKeys` 写它 —— 属既有错误，"这把 Key 还剩多少额度"目前查不出来。
> ③ 「渠道全部可用模型」只能进 `models`，而它强制 `max_input_tokens`/`max_output_tokens`（§2bis 预留上界的硬前置）；20 渠道 × 200~300 模型不可能手填。
>
> **最小设计原则**（ponytail 决策阶梯）：只建 P1 有消费者的列。高峰倍率、独占标记、平台归属、自报能力位、`enabled_model_id` 等 11 个字段**刻意不建** —— 它们的消费者都在 P2/P3 调度，需要时 `ALTER ADD COLUMN` 无损。自报能力位另有一层理由：P2 本就要用 `Probe()` 实测（[03 §8](./03-upstream-layer.md)），现在采自报值等于存一份将被推翻的数据。

```sql
-- 渠道分组（FR-123）：倍率 / 限流 / 可用模型的载体
CREATE TABLE channel_groups (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  channel_id    BIGINT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  group_ref     TEXT NOT NULL,               -- 上游分组标识（NewAPI group / Sub2API group_id）
  rate_multiplier NUMERIC(12,6),             -- 分组倍率（采集所得；历史版本仍走 multiplier_versions）
  data_source   TEXT NOT NULL CHECK (data_source IN ('auto_collect','manual')),
  fetched_at    TIMESTAMPTZ NOT NULL,        -- 陈旧性查询期计算，不存 is_stale（与 §7 同一做法）
  UNIQUE (channel_id, group_ref)
);

-- 分组可用模型（FR-124）：用上游原始模型名，**不要求已登记进 models**
-- 这是它不能借道 channel_models 的原因——后者的 model_id 外键指向 models
CREATE TABLE group_models (
  channel_group_id BIGINT NOT NULL REFERENCES channel_groups(id) ON DELETE CASCADE,
  model_name    TEXT NOT NULL,
  fetched_at    TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (channel_group_id, model_name)
);

-- 渠道模型目录（FR-126）：上游声称可用的全部模型，**无 token 上界约束**
-- 与 models/channel_models（可路由模型）是两层：目录=上游有什么，可路由=我们决定用什么
CREATE TABLE channel_model_catalog (
  channel_id    BIGINT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  model_name    TEXT NOT NULL,               -- 上游原始名
  input_price   nonneg_usd,                  -- 采到的价格，供选型参考（权威价仍在 price_versions）
  output_price  nonneg_usd,
  -- billing_unit：上面两列的**口径**（第 46 轮真实数据补，见下方说明）
  billing_unit  TEXT CHECK (billing_unit IN
                  ('per_1m_token','per_1k_token','per_token','per_call')),
  first_seen_at TIMESTAMPTZ NOT NULL,
  last_seen_at  TIMESTAMPTZ NOT NULL,        -- 停止更新 = 上游下架了它（FR-126 告警判据）
  PRIMARY KEY (channel_id, model_name)
);

-- upstream_keys 补 7 列（FR-122/125/127）
ALTER TABLE upstream_keys
  ADD COLUMN channel_group_id BIGINT REFERENCES channel_groups(id),  -- Key 归属分组（FR-123）
  ADD COLUMN remain_quota_usd nonneg_usd,     -- 剩余额度（归一美元，FR-125）
  ADD COLUMN used_quota_usd   nonneg_usd,     -- 已用额度（FR-125）
  ADD COLUMN rpm_limit        INTEGER,        -- 上游 Key 级 RPM（FR-127；**P1 只存不判**）
  ADD COLUMN concurrency_limit INTEGER,       -- 上游 Key 级并发（FR-127；同上）
  ADD COLUMN quota_synced_at  TIMESTAMPTZ,    -- 最近同步时刻（陈旧判定，FR-128）
  -- external_ref：上游侧的 Key 标识（第 46 轮补，实现时发现缺）。
  -- 采集只能拿到上游的 key id/name（脱敏引用），**拿不到明文 secret**，
  -- 故无法用 secret 反查库中是哪一行 —— 没有这一列，"把采到的用量写回对应
  -- Key"就无从落地，而那正是 FR-125 的核心。
  -- 唯一性作用域是**账号**而非渠道：同一渠道下不同账号在上游各有独立的 id 空间。
  ADD COLUMN external_ref    TEXT;
```

**索引**

```sql
CREATE INDEX idx_chgroups_channel ON channel_groups(channel_id);
CREATE INDEX idx_catalog_channel  ON channel_model_catalog(channel_id, last_seen_at DESC);
CREATE INDEX idx_keys_group       ON upstream_keys(channel_group_id);
CREATE UNIQUE INDEX idx_keys_external_ref
  ON upstream_keys (account_id, external_ref) WHERE external_ref IS NOT NULL;
```

#### 1.3bis 三张表的写入语义（**P1 必需**，第 45 轮补）

> ⚠️ **此前"采不到即删行"只是砍掉 `available` 列的理由，不是实现规则** —— 先删后插还是 diff？什么事务包住？两次 sync 并发会不会互相删？`first_seen_at`/`last_seen_at` 的 upsert 也只有一句括号说明。开发无从下笔（开发视角审查第 45 轮）。

**`group_models`：按分组全量替换，单事务**

```sql
-- 在 sync 的 ③ 分组事务内，逐个 channel_group_id 执行
BEGIN;
DELETE FROM group_models WHERE channel_group_id = :gid;
INSERT INTO group_models (channel_group_id, model_name, fetched_at)
SELECT :gid, unnest(:model_names::text[]), :fetched_at;
COMMIT;
```

- **全量替换而非 diff**：分组可用模型是**上游的完整声明**，diff 需要额外判断"这次没返回"是"下架了"还是"接口抽风"——而全量替换配合"采集失败则整项 `failed`、不进事务"已经表达了正确语义：**要么用这次的完整快照，要么保留上一次的**。
- **并发安全**：③ 已被 `sync` 的渠道级 advisory lock 串行化（[09 §5.0bis](./09-admin-api.md)），同渠道不会有两个 sync 同时删同一分组。
- **空结果的处理**：若上游明确返回"该分组零个可用模型"，则删完不插——这是合法状态。但**若采集报错，整项 `failed`、事务不提交**，旧行保留。

**`channel_model_catalog`：upsert，`first_seen_at` 只在插入时写**

```sql
INSERT INTO channel_model_catalog
       (channel_id, model_name, input_price, output_price, billing_unit,
        first_seen_at, last_seen_at)
VALUES (:cid, :name, :in_price, :out_price, NULLIF(:unit,''), :now, :now)
ON CONFLICT (channel_id, model_name) DO UPDATE
   SET input_price   = EXCLUDED.input_price,
       output_price  = EXCLUDED.output_price,
       billing_unit  = EXCLUDED.billing_unit,
       last_seen_at  = EXCLUDED.last_seen_at;
       -- ⚠️ **不更新 first_seen_at** —— 它记录"首次见到"，被覆盖就永久丢失
```

**`billing_unit` 为何必需**（第 46 轮实测发现，不是理论洁癖）

同一个 NewAPI 的 `/api/pricing` 里**混着两种口径**，靠 `quota_type` 区分：

| `quota_type` | 取价字段 | 语义 | 口径 |
| --- | --- | --- | --- |
| `0` | `model_ratio` | 相对基准价的**倍率** | `per_1m_token` |
| `1` | `model_price` | 每次调用的**绝对美元价** | `per_call` |

实测某真实站点 1369 个模型中 **208 个（15%）是按次计价**，而两种口径的**数值区间重叠**——按次价样本 `0.15 / 0.56 / 0.22 / 0.08`，倍率样本 `30 / 2 / 0.685`。因此**无法从数值反推口径**：把 `$0.15/次` 当成倍率 `0.15` 参与成本排序，会让按次计价的模型显得比实际便宜若干个数量级，而这类模型（绘图、视频）往往恰恰是最贵的。

这就是 [§3](#3-计价与账务) 与 [#7](https://github.com/) 一直强调的 `billing_unit` 缩放风险，只不过它先在 P1 的目录表上现形，而不是等到 P3 的成本计算。

- **无价则口径留 NULL**：Sub2API/ASXS 是 degraded 站型、单价一律缺失，**不补默认值**。补 `per_1m_token` 会把"上游未声明"伪装成"已知按 token 计价"；NULL 才让消费方按未知处理。
- **消费方义务**：读 `input_price` 前必须先读 `billing_unit`，缺失时**不得**假定任何默认口径。

- **不删除消失的模型**：这是与 `group_models` 相反的选择。目录要回答"上游曾经有什么、什么时候消失的"（FR-126 下架识别），删掉行就无从判断——`last_seen_at` 停止前进**本身就是下架信号**。
- **下架判据**：`last_seen_at` 连续 `catalog_missing_rounds`（默认 3，见 [09 §4bis](./09-admin-api.md)）轮采集未前进 → 视为下架，触发 P3 告警（AC-40）。用"轮数"而非"时长"是因为采集周期可配，轮数对周期变化免疫。

**`upstream_keys` 的用量列：只更新、不插入**

- Key 行由 `/admin/keys` 人工登记（我们持有的凭证不可能从上游"发现"），采集只 `UPDATE` 用量列 + `channel_group_id` + `quota_synced_at`。
- **采集到一把库里没有的 Key**（运营在上游侧新建但没登记）：**不自动插入**，记 `collector_snapshots(scope_type='key')` 并在 `inventory` 的异常项计数里 +1 提示运维补登记。理由：`upstream_keys.secret` 是明文凭证，上游列表接口通常只回前缀或掩码，**凭空插一行没有 secret 的 Key 会让它永远不可用**且污染资产台账。

**几处刻意的取舍**

| 决定 | 理由 |
| --- | --- |
| Key 用量**历史**不建新表 | 复用已有 `collector_snapshots`（`scope_type='key'` + `payload` JSONB + 三元组 + 保留策略 + 索引全都在）。`upstream_keys` 六列只存**当前值**供列表展示 |
| Key 归属分组用**单列**而非关联表 | 20 个渠道均为中转站，Key 建好后分组基本固定。多对多要传导到 `bindings` 唯一性定义与健康统计，代价不对等 |
| `group_models` 不设 `available` 布尔 | 采到即可用，采不到即删行。恒为 true 的列没有信息量 |
| 目录不存 `cache_price`/`billing_unit` | 权威价在 `price_versions`（不可覆盖版本，FR-012）；目录两列只为"看一眼贵不贵" |
| 目录不设 `enabled_model_id` | P1 无路由，"启用模型"这个动作不存在。P2 要启用时按 `(channel_id, model_name)` 匹配 `models.canonical_name` 即可 |
| **`topup_rate` 不建**（充值倍率） | 唯一消费者是成本排序，P1 无成本排序。**P3 必建**——不建则 1:2 充值渠道成本被高估 2 倍（[ISSUE-005 §6 T-1](../issues/ISSUE-005-phase1-upstream-inventory.md)） |

**服务 FR/AC**：FR-122~128；AC-37、AC-38、AC-39、AC-40。

---

## 2. 别名与策略域（对外别名即策略载体）

> FR-062/117：调用方通过选择**对外模型别名**选择策略（SLA 等级、是否允许测活）。业务不逐请求标注敏感属性。策略需版本化可回滚（FR-104）、全部可配置 + 关键项二次确认（FR-115）。

```sql
-- ⚠️ routing_policies 必须先于 model_aliases 创建（后者有外键指向它），
-- 定义见下方"路由策略"；此处仅提示建表顺序，实际 DDL 在 migrations/ 中按依赖排序。

-- 路由策略（FR-090/104/115）：一套具体策略，别名映射到它
CREATE TABLE routing_policies (
  id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name           TEXT NOT NULL,               -- 如 sla-gold / sla-silver / sla-bronze（仅默认模板，可自定义）
  sla_level      TEXT NOT NULL,               -- 等级名可配置（DECISIONS 参数1：等级数量/数值全自定义）
  -- ⚠️ 调度与实验流量判断**一律读下面三个显式字段，不读 sla_level 名字**（2026-07-26 决议）：
  --    一期"单级 SLA"= 单一**承诺**等级；金/银/铜只是 M0 默认模板名，不承载语义。
  --    二期加等级时结构零改动。
  is_committed     BOOLEAN NOT NULL DEFAULT false, -- 该策略对外有 SLA 承诺（须有对应 sla_targets 行）
  canary_eligible  BOOLEAN NOT NULL DEFAULT false, -- 可被 canary 放量（FR-121）
  probe_allowed  BOOLEAN NOT NULL DEFAULT false,   -- 可被主动测活选中（FR-060~067）
  -- 承诺别名**永不进任何实验流量**：canary 与主动测活都挡掉。
  -- 只写 NOT(canary_eligible AND is_committed) 不够——那样承诺别名仍可能被主动测活选中。
  CONSTRAINT committed_excludes_experiments
    CHECK (NOT (is_committed AND (canary_eligible OR probe_allowed))), -- 该别名是否允许测活（FR-062，*-sla-* 默认禁测活）
  -- 冲突优先序可配置（DECISIONS 参数2）：默认 金 SLA>缓存>成本…
  conflict_order JSONB NOT NULL DEFAULT '["sla","cache","cost"]',
  -- 全渠道不可用排队上限（§11 默认：金15s/银5s/铜0s，可配置）
  no_resource_wait_ms INTEGER NOT NULL DEFAULT 0,
  version        INTEGER NOT NULL DEFAULT 1,  -- FR-104 版本
  effective_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  changed_by     TEXT,                        -- FR-099/104 责任人
  change_reason  TEXT,
  is_active      BOOLEAN NOT NULL DEFAULT true
);

-- 策略变更影子表（FR-104 的"可恢复到上一个已确认版本"，第 42 轮补）
-- ⚠️ 为什么不做成「主表插新版本」：routing_policies 有两个外键指向它
--    （model_aliases.policy_id、requests.policy_id）。插新版本会让别名永远
--    指着旧行，第一次改策略之后每个请求都命中一个已下线的策略。
--    影子表让主表保持**单行身份**（外键安全）而历史**只追加**（可回滚）。
CREATE TABLE routing_policy_revisions (
  id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  policy_id      BIGINT NOT NULL REFERENCES routing_policies(id),
  version        INTEGER NOT NULL,            -- 快照对应的主表 version（改**前**的值）
  -- 改前的全部可变字段快照；回滚 = 把这些抄回主行
  sla_level      TEXT NOT NULL,
  is_committed   BOOLEAN NOT NULL,
  canary_eligible BOOLEAN NOT NULL,
  probe_allowed  BOOLEAN NOT NULL,
  conflict_order JSONB NOT NULL,
  no_resource_wait_ms INTEGER NOT NULL,
  is_active      BOOLEAN NOT NULL,
  changed_by     TEXT,                        -- 是谁把它改走的（FR-099/104）
  change_reason  TEXT,
  superseded_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (policy_id, version)                 -- 同一版本只留一条快照
);
CREATE INDEX idx_polrev_latest ON routing_policy_revisions(policy_id, version DESC);

-- 对外模型别名（FR-062/117、AC-25）：如 gpt-5.5（可测活） vs gpt-5.5-sla-1（禁测活）
CREATE TABLE model_aliases (
  id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  alias          TEXT NOT NULL UNIQUE,        -- 对外暴露名（= 调用方 body.model 里填的字符串）
  -- ⚠️ 必须 NOT NULL（第 40 轮）：别名的**全部作用**就是 ①定策略 ②定模型。
  --    可空意味着可以建出"指不到任何模型的别名"，selector 序 2 拿不到 model_id 无法过滤。
  --    别名解析规则见 [05 §1.0](./05-scheduling-and-operations.md)。
  target_model_id BIGINT NOT NULL REFERENCES models(id),
  policy_id      BIGINT NOT NULL REFERENCES routing_policies(id),
  enabled        BOOLEAN NOT NULL DEFAULT true,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- SLA 目标（FR-090/091）：按模型/请求类型/租户/等级+统计周期配置服务目标，全部可配置
CREATE TABLE sla_targets (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  policy_id    BIGINT NOT NULL REFERENCES routing_policies(id),
  model_id     BIGINT REFERENCES models(id),
  request_type TEXT,                          -- chat/responses/tool… NULL=通配
  tenant_id    TEXT,                          -- 个人/内部默认单租户，可空
  metric       TEXT NOT NULL,                 -- availability / ttft_p95 / ttft_p99 / stream_break / cache_hit …
  target_value NUMERIC(20,6) NOT NULL,        -- 目标值（如 ttft_p95=10s、cache=0.90）
  window_spec  TEXT NOT NULL                  -- 统计周期，如 '1h'/'1d'/'7d'
);

-- 可配置参数总表（FR-104/115/116）：策略/时效/门槛统一登记；关键项修改需二次确认
CREATE TABLE config_params (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  scope_type    TEXT NOT NULL CHECK (scope_type IN ('global','tenant','channel','policy','model')),
  -- ⚠️ 不可为 NULL：PG 普通 UNIQUE 允许多行 NULL，global 用 NULL 会让唯一约束失效，
  -- 并发/重试提交可造出同一参数的重复版本（对抗性审查发现）。global 统一用固定哨兵值 '*'
  scope_id      TEXT NOT NULL DEFAULT '*',    -- 非 global 填对应标识；global 恒为 '*'
  param_key     TEXT NOT NULL,                -- 如 probe.budget.global_pct / sample.min_1h / cooldown.base_sec
  param_value   JSONB NOT NULL,
  is_critical   BOOLEAN NOT NULL DEFAULT false, -- 关键策略：测活预算/禁测活/订阅倾斜/旁路开关（1.4/FR-115）
  version       INTEGER NOT NULL DEFAULT 1,
  effective_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  prev_value    JSONB,                        -- FR-099 前后值
  changed_by    TEXT,
  change_reason TEXT,
  confirmed_twice BOOLEAN NOT NULL DEFAULT false, -- is_critical 时必须为 true 才生效（二次确认）
  -- 并发控制（09 §3 apply 流程配套）：幂等键使重复提交只生效一次
  idempotency_key TEXT,
  CONSTRAINT config_scope_global_sentinel
    CHECK (scope_type <> 'global' OR scope_id = '*'),
  UNIQUE (scope_type, scope_id, param_key, version),
  -- ✅ 同样刻意用普通 UNIQUE：`idempotency_key` 为 NULL 表示调用方未启用幂等，
  --    此时允许同一 param_key 多次变更（幂等是 opt-in）。勿改 NULLS NOT DISTINCT。
  UNIQUE (param_key, idempotency_key)          -- 同一参数的同一幂等键只允许一行
);

-- 版本分配与并发提交的约定（详见 [09 §3](./09-admin-api.md)）：
--   apply 必须携带 expected_current_version 与 idempotency_key；
--   服务端在**单个事务**内完成：① 锁定该 (scope_type,scope_id,param_key) 的当前最大版本
--   ② 比对 expected_current_version，不匹配则拒绝（409，preview 已过期）
--   ③ 分配 version = max+1 并插入 ④ 消费 confirm_token。
--   → 两个基于同一旧版本的并发 apply，只有一个成功；重复投递因幂等键被去重。
```

**索引**

```sql
CREATE INDEX idx_aliases_policy   ON model_aliases(policy_id) WHERE enabled;
CREATE INDEX idx_slatargets_pol   ON sla_targets(policy_id, model_id);
CREATE INDEX idx_config_active    ON config_params(scope_type, scope_id, param_key) WHERE version IS NOT NULL;
```

**服务 FR/AC**：FR-062、FR-090/091、FR-104、FR-115/116/117；AC-25（可测活 vs 禁测活别名）、AC-26（协议由 request_type 承载）。

---

## 2bis. 网关调用方凭证域（**入站鉴权**，对抗性审查新增）

> **此前的空白**：设计里唯一的密钥模型是 `upstream_keys.secret`——那是**我们打上游用的**。`/v1/*` 的入站鉴权从未定义，开发只能二选一：复用上游 Key（把高价值凭证暴露给调用方）或不鉴权（**任何能访问端口的人都能烧额度**）。两者都不可接受。

**铁律：`upstream_keys.secret` 严禁用作入站凭证。** 入站与出站是两套完全独立的凭证体系。

```sql
-- 网关调用方凭证（入站）：只存哈希，永不存明文
CREATE TABLE gateway_clients (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name          TEXT NOT NULL,                 -- 调用方标识（如 "codex-cli-local"）
  tenant_id     TEXT,                          -- 关联租户（个人场景可空）
  -- 数据许可属性（FR-093，一期不启用时全为 NULL → 求值按 '*' 处理，见 §2ter）
  region        TEXT,                          -- 地区（如 'us'/'cn'）；NULL=不限
  business_tier TEXT,                          -- 业务等级（如 'prod'/'dev'）；NULL=不限
  data_class    TEXT,                          -- 数据级别（'public'/'internal'/'sensitive'）；**服务端唯一可信来源**，
                                               -- 严禁由请求头覆盖（见 §2ter）；NULL=不限
  -- ⚠️ 与上游 Key 不同：入站凭证**只存哈希**（Argon2id/bcrypt），明文仅在签发时返回一次
  secret_hash   TEXT NOT NULL,
  secret_prefix TEXT NOT NULL,                 -- 明文前 8 位，供展示与定位（如 "gw-a1b2c3"）
  -- 授权范围
  allowed_aliases TEXT[],                      -- 可用的模型别名；NULL=全部（FR-062/117）
  -- 配额（防单个调用方烧光额度）
  quota_daily_usd nonneg_usd,                  -- 日费用上限；NULL=不限
  rpm_limit     INTEGER,                       -- 每分钟请求数上限；**NULL = 不限速**（见下方 B 阶段：
                                               -- NULL 时必须**整段跳过** RPM 原子闸，不可代入 SQL——
                                               -- `count < NULL` 恒为 UNKNOWN → DO UPDATE 不执行 → 第二个请求起全部误 429）
  -- 生命周期
  status        TEXT NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active','revoked','expired','system_internal')),
                  -- system_internal：内置凭证，**鉴权层硬拒外部调用**（只能由 steward 在进程内使用）
  is_system     BOOLEAN NOT NULL DEFAULT false,  -- 保护标记：/admin/clients 的吊销/轮换/删除**必须拒绝**它
                                                 -- 否则一次误操作就让主动测活整体失效
  expires_at    TIMESTAMPTZ,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at    TIMESTAMPTZ,
  revoke_reason TEXT,
  last_used_at  TIMESTAMPTZ,                   -- 审计：最近使用时间
  UNIQUE (secret_prefix)
);

CREATE INDEX idx_gwclient_active ON gateway_clients(status) WHERE status='active';
```

**跨实例配额执行**（对抗性审查发现：光有 `quota_daily_usd`/`rpm_limit` 字段无法执行——双 core 各自计数会被绕过，并发请求可在费用落账前同时通过检查）：

```sql
-- 日费用预留账：按 (client, 日期) 原子累加，多实例共享
CREATE TABLE client_daily_spend (
  gateway_client_id BIGINT NOT NULL REFERENCES gateway_clients(id),
  spend_date    DATE NOT NULL,
  reserved_usd  nonneg_usd NOT NULL DEFAULT 0,  -- 已预留（请求发起时 +预估）
  settled_usd   nonneg_usd NOT NULL DEFAULT 0,  -- 已结算（请求结束时按实际替换预留）
  PRIMARY KEY (gateway_client_id, spend_date)
);

-- RPM 滑动窗口：同样落库，避免实例本地状态被 LB/重启绕过
CREATE TABLE client_rate_window (
  gateway_client_id BIGINT NOT NULL REFERENCES gateway_clients(id),
  window_start  TIMESTAMPTZ NOT NULL,           -- 分钟粒度对齐
  request_count INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (gateway_client_id, window_start)
);
```

**⚠️ 结算必须幂等**（对抗性审查 critical 发现）：outbox 投递天生可重放（在"已应用更新、尚未标记 delivered"之间崩溃后必然重发），若结算是**直接对聚合计数器做加减**（`reserved_usd -= x`），重放即**二次结算** → 负预留或虚高消费，进而 429 判错。故引入 per-request 预留账：

```sql
-- 每请求一行预留记录：结算以此为准，聚合表由它派生
CREATE TABLE client_reservations (
  request_id    UUID PRIMARY KEY,               -- 天然幂等键：一个请求只有一行
  gateway_client_id BIGINT NOT NULL REFERENCES gateway_clients(id),
  spend_date    DATE NOT NULL,
  estimated_usd nonneg_usd NOT NULL,            -- 请求进入时预留
  actual_usd    nonneg_usd,                     -- 结算后写入
  state         TEXT NOT NULL DEFAULT 'reserved'
                  CHECK (state IN ('reserved','settled','abandoned')),
  settle_event_key TEXT,                        -- 结算事件唯一键，重放去重
  needs_manual_review BOOLEAN NOT NULL DEFAULT false, -- unknown_billing/interrupted 结算：金额为保守估算，待人工核对
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  settled_at    TIMESTAMPTZ,
  -- ✅ 此处**刻意**用普通 UNIQUE（允许多行 NULL）：未结算的 reservation 该列恒为 NULL，
  --    改成 NULLS NOT DISTINCT 会导致全局只允许存在一条未结算预留 —— 直接锁死系统。
  --    与 bindings/health_metric_windows 的情形相反，勿一并"修正"（第 19 轮自查澄清）。
  UNIQUE (settle_event_key)
);
-- 运维视图：所有待人工核对的保守结算
CREATE INDEX idx_resv_review ON client_reservations(gateway_client_id, spend_date)
  WHERE needs_manual_review;
CREATE INDEX idx_resv_open ON client_reservations(gateway_client_id, spend_date)
  WHERE state = 'reserved';
```

**预估上界算法**（对抗性审查第 7 轮 [high] 引入、第 9 轮 [critical] 修正）：

> ⚠️ **`len(RawBody)` 不是通用上界**（第 9 轮）：设计承诺多模态原样透传——一个几十字节的图片 URL 可对应上千视觉计费 token；Responses 的 `previous_response_id`/`conversation` 还会引入**请求体里根本不存在**的服务端上下文。这两类请求下按字节数预留会严重低估，日配额照样被突破。

```
单跳上界(binding) = ( 输入 token 上界 × pv.input_price
                    + models.max_output_tokens × pv.output_price ) / u × m
      -- pv / u / m 与 §3 成本公式同源同定义（当前生效价版本、billing_unit 缩放、两个倍率之积）
      -- ⚠️ 预留**不用** cache_price：预留是上界，必须假设最坏情况（零缓存命中）

输入 token 上界 =
  ├─ 纯文本且无服务端上下文引用 → min( len(RawBody), models.max_input_tokens )
  │     依据：字节级 BPE 每 token 至少映射 1 字节 → token 数 ≤ 字节数，恒成立
  └─ 其余情况（保守分支）      → models.max_input_tokens
        依据：上游**物理上不可能**计费超过模型上下文窗口 → 对任意输入都是真上界

estimated_usd = Σ 单跳上界(b)  for b in RoutePlan        -- 按全部跳求和（接管会真的多次计费）
```

**分支判定（字节级子串扫描，不做 JSON 解析）**：请求体中出现以下任一标记即走保守分支——
`"image_url"`、`"input_image"`、`"input_audio"`、`"input_file"`、`"file_id"`、`"previous_response_id"`、`"conversation"`。
**扫描不确定时一律走保守分支**（宁可多预留，不可少预留）。

- **硬前置**：`models.max_input_tokens` 或 `max_output_tokens` 为 NULL 的模型，其 binding **不得进入候选**（与协议能力探测同级的登记项）。理由：字节透传硬约束禁止我们改写请求体注入上限，上界只能来自登记值；**无上界即必须拒绝，不能猜**。
- **请求体自带更小的 `max_output_tokens`/`max_tokens` 不采信**（我们不解析正文），恒用登记上限，只会更保守。
- **代价（明示）**：多模态与续接会话请求会按**整个上下文窗口**预留，日配额利用率显著下降。这是刻意选择——超限不可逆，过度预留只是暂时占用（结算即释放差额）。若该调用方以多模态为主且不希望被过度限制，**把它的 `quota_daily_usd` 置 NULL**（不设日配额，只记账不预留），由运维用告警而非硬闸控制。

**实际费用超出预留时**（上游不遵守上限等）：结算按**实际值**写 `settled_usd`。溢出后的拒绝**无需任何新字段**——D 阶段的原子预留语句本身就带 `reserved_usd + settled_usd + :est <= :quota` 条件，`settled_usd` 一旦超出，下一个请求的 UPDATE 自然返回 0 行 → **429**。
  > ⚠️ 曾写"立即置该 client 当日 `over_quota`"——`client_daily_spend` 与 `gateway_clients` **都没有这一列**（第 17 轮 [high]），照此实现会写不存在的字段。**状态由 `reserved_usd + settled_usd >= quota_daily_usd` 派生**，不落列、无需维护一致性。

  **已完成的请求不追溯拒绝**（无法收回）。此为可接受的有界溢出，AC-33 须断言"溢出后下一请求必被拒"。

**写入路径分工（唯一口径，第 15 轮 [critical] 统一）**：

| 事实 | 写法 | 时机 |
| --- | --- | --- |
| `attempt_status='committed'`、`response_committed_at`、`has_ttft_output`、`content_aware_ttft_ms` | **同步直写 `attempts`** | 放行首字节**之前** |
| `terminal_event`、`attempt_usage`、reservation 结算、attempt 终态（= `finalize_upstream`） | **同步直写表，一个事务** | 放行终帧字节**之前** |
| `downstream_first_byte_written_at` | 经 outbox | socket write 返回后 |
| `downstream_write_completed_at` + `requests.final_status`（= `finalize_delivery`） | 经 outbox | socket write 返回后 |
| **`attempt_status`(终态) + `cancel_reason` + `canceled_by_sla`** | **同步直写 `attempts`** | **每跳取消/结束时** |
| `cancel_propagated` 等纯观测字段 | 经 outbox | 每跳取消后 |

> ⚠️ 曾有三份文档三种说法（01 说"同步提交 terminal_event/usage/attempt_end"、03 说"同步写 durable outbox"、02 说 `finalize_upstream` 直写表），开发无从判断终帧事实到底进表还是进 outbox。**以本表为唯一口径**：需要"先落库再放行字节"的事实一律**同步直写表**（outbox 多一跳投递，无法满足该顺序）；outbox 只留字节写出之后才产生的异步事实。

**统一终结事务 `finalize(request_id, outcome)`**（**唯一被允许修改聚合计数器的代码路径**）：

> 对抗性审查第 7 轮 [critical]：预留在请求发起**前**创建，而"上游未发出即崩溃"的恢复只把 attempt 置 `failed`，**没有释放预留** → 每次这类崩溃永久占用一份日配额，累积到最后所有合法请求都收 429。人工把聚合值改回去又重新引入重复扣减。故三条路径必须共用同一个幂等事务。

```sql
BEGIN;
-- ⚠️ 第 21 轮 [critical]：上一版只有 ② 受 `settled` 约束，③/③bis/④/⑤ 是独立语句 →
--    并发场景下 `finalize_upstream` 已把 reservation 结算掉之后，一个迟到的 `finalize_abort`
--    的 ① 影响 0 行（正确），却仍然会执行 ⑤ 把 `requests.final_status` 从 pending 改成
--    failed/canceled —— 直接破坏 K4 与正常交付分支。
-- 修法：整条链全部 `FROM settled` 驱动，闸门不通过则**没有任何一句执行**。
WITH settled AS (
  -- ① 唯一闸门：per-request 行的状态跃迁。重放/并发时返回 0 行 → 下面全部不执行
  UPDATE client_reservations
     SET state            = :new_state,      -- 'settled' | 'abandoned'
         actual_usd       = :actual_usd,
         settle_event_key = :event_key,
         needs_manual_review = :needs_review,
         settled_at       = now()
   WHERE request_id = :rid AND state = 'reserved'
  RETURNING gateway_client_id, spend_date, estimated_usd, actual_usd
),
agg AS (
  -- ② 聚合扣回：client/日期/预留额**全部取自 settled**，不接受调用方传参
  UPDATE client_daily_spend d
     SET reserved_usd = d.reserved_usd - s.estimated_usd,
         settled_usd  = d.settled_usd  + COALESCE(s.actual_usd, 0)
    FROM settled s
   WHERE d.gateway_client_id = s.gateway_client_id AND d.spend_date = s.spend_date
  RETURNING 1
),
req AS (
  -- ⚠️ 第 22 轮 [high]：`att` 原先只按 `a.id=:attempt_id` 更新，**不校验它属于 :rid** →
  --    参数错配时会「结算 A 请求、终结 B 请求的 attempt」。故先锁 requests 派生分区键。
  SELECT id, created_at FROM requests WHERE id = :rid AND created_at = :rcat FOR UPDATE
),
att AS (
  -- ③ 本跳 attempt 终态（按 attempt_id，且**必须校验归属**；不可按 request_id 批量——多跳会误伤历史跳）
  UPDATE attempts a
     SET attempt_status = :attempt_terminal,
         terminal_event = :terminal_event,
         upstream_terminal_at = :upstream_terminal_at,
         content_aware_ttft_ms = COALESCE(a.content_aware_ttft_ms, :ttft_ms),
         ended_at = now()
    FROM settled, req
   WHERE a.id = :attempt_id
     AND a.request_id = req.id AND a.request_created_at = req.created_at   -- 归属校验
     AND a.attempt_status IN ('pending','committed')
  RETURNING a.id, a.request_created_at
),
usage_ins AS (
  -- ③bis usage 同事务落库；**必须显式带 cost_source**（第 21 轮 [high]：
  --      列默认 'upstream'，漏写会把估算值伪装成上游真实费用，污染成本报表与人工核对）
  INSERT INTO attempt_usage (id, attempt_id, request_created_at, upstream_seq,
                             prompt_tokens, completion_tokens, total_tokens,
                             prompt_cached_tokens, total_cost, estimated_cost,
                             cost_variance, cost_source)
  SELECT :usage_id, att.id, att.request_created_at, 1,   -- 分区键取自 att，不再单独传参
         :prompt, :completion, :total, :cached, :cost, :est, :cost - :est,
         :cost_source                       -- 'upstream' | 'estimated'，由调用方按是否收到真实 usage 决定
    FROM att                                -- 仅当 ③ 生效才插
  ON CONFLICT (attempt_id, request_created_at, upstream_seq) DO NOTHING
  RETURNING id
),
-- ④ 释放本跳的**三套**占用（第 31 轮统一：此前只放 canary，capacity/probe 永不释放必然泄漏）
rel_cap AS (
  UPDATE capacity_claims c SET state='released', released_at=now()
    FROM settled WHERE c.attempt_id = :attempt_id AND c.state='active' RETURNING c.binding_id),
rel_can AS (
  UPDATE canary_claims  c SET state='released', released_at=now()
    FROM settled WHERE c.attempt_id = :attempt_id AND c.state='active' RETURNING c.binding_id),
rel_prb AS (
  UPDATE probe_claims   c SET state='released', released_at=now()
    FROM settled WHERE c.attempt_id = :attempt_id AND c.state='active' RETURNING c.binding_id),
cap_agg AS (SELECT binding_id, count(*) AS n FROM rel_cap GROUP BY binding_id),
can_agg AS (SELECT binding_id, count(*) AS n FROM rel_can GROUP BY binding_id),
dec AS (
  UPDATE resource_health h
     SET concurrency_inflight = GREATEST(h.concurrency_inflight - COALESCE(c.n,0), 0),
         canary_inflight      = GREATEST(h.canary_inflight      - COALESCE(k.n,0), 0)
    FROM (SELECT binding_id FROM cap_agg UNION SELECT binding_id FROM can_agg) b
    LEFT JOIN cap_agg c ON c.binding_id = b.binding_id
    LEFT JOIN can_agg k ON k.binding_id = b.binding_id
   WHERE h.binding_id = b.binding_id
  RETURNING 1)
-- ⑤ request 终态：**仅 finalize_abort / finalize_recovery 传非空 :request_terminal**；
--    finalize_upstream 传 NULL，本句自然影响 0 行（那时交付结果未知，写了会让 K4 不可达）
, upd AS (
  UPDATE requests r SET final_status = :request_terminal
    FROM settled
   WHERE r.id = :rid AND r.final_status = 'pending' AND :request_terminal IS NOT NULL
  RETURNING r.id)
SELECT (SELECT count(*) FROM settled) AS settled_n,
       (SELECT count(*) FROM att)     AS att_n,
       (SELECT count(*) FROM upd)     AS request_updated;
-- ⚠️ **块内不写 COMMIT**（第 34 轮修正：原版紧跟 COMMIT，与下方"应用层按三态
--    COMMIT/ROLLBACK"直接矛盾——先提交就没得选了）。且原版末尾是裸 UPDATE 无
--    RETURNING，`finalize_upstream` 传 NULL 终态时它影响 0 行，应用层拿不到任何计数。
--    提交与否一律由应用层依下表三态判定。
```

**每跳结束必须同步写该跳终态**（本轮自查发现）：SLA 接管取消 hop1 时，`attempt_status='canceled_by_sla'` 与 `cancel_reason='sla_takeover'` **必须同步直写**，不能只经 outbox 异步记 `cancel_reason`——否则 hop2 关单时 hop1 仍是 `committed`，任何批量语句都会误判它。`cancel_propagated` 等纯观测字段仍可异步。

> **`finalize` 的入参**：`request_id`、`request_created_at`（分区键，锁 requests 用）、`attempt_id`、`new_state`、`actual_usd`、`event_key`、`needs_review`、`terminal_event`、`upstream_terminal_at`、`ttft_ms`、`cost_source` 与 usage 各列、两个终态。
> **末尾须 `RETURNING`，由应用层按三态判定**（第 23 轮 [high]：原文写"断言 `settled=1 AND att=1` 否则 ROLLBACK"，
> 与 [AC-33](./14-acceptance-matrix.md) 要求的"同一 `settle_event_key` 重放、连接中断后重试须幂等"**直接矛盾**——
> 已提交后的重试必然 `state≠'reserved'`，两个计数都是 0，按原文会被当成异常告警而不是幂等成功）：
>
> ⚠️ **必须同时看 `settled_n` 与 `att_n`**（第 36 轮）：只看 `settled` 时，
> `settled_n=1 且 att_n=0` 会**提交 reservation 与聚合、却没终结 attempt/usage**——
> 钱结了、单没关，恢复扫描随后又会把它当悬挂处理。
>
> | `settled_n` | `att_n` | 判定 | 处置 |
> | --- | --- | --- | --- |
> | 1 | 1 | `applied` | COMMIT |
> | 1 | 0 | **异常**（attempt 不存在 / 不属该 request / 已终态） | **ROLLBACK + P2 告警** |
> | 0 | — | 该 reservation 已终态且 `settle_event_key` 与本次相同 → `already_applied_same_event` | COMMIT（幂等成功，**不告警**） |
> | 0 | — | 不满足上一行 → `conflict_or_not_found` | **ROLLBACK + P2 告警** |
>
> **claim 释放同受此约束**：`att_n=0` 时整事务回滚，三张 claim 表自然不会被误放
> （否则会出现"attempt 还在跑、占用已释放"，容量与 canary 计数双双失真）。
>
> CTE 不会因某一步 0 行而自动回滚，故提交与否**必须由应用层依上表决定**。**`gateway_client_id`/`spend_date`/`estimated_usd` 一律由 `RETURNING` 派生**——与 `adjust` 同一条纪律。跨午夜的长请求因此天然把费用记在**预留那一天**。

**三个终结入口，覆盖全部收尾路径**（第 17 轮 [critical]：此前只定义了"终帧到达"这一条正常路径，**取消/断流/超时结束的请求没有任何终结事务**，会残留 `pending` + `reserved`；恢复扫描也没有写 `requests.final_status` 的载体）：

| 入口 | 触发 | 写 attempt | 写 request | 写 reservation | canary claim |
| --- | --- | --- | --- | --- | --- |
| **`finalize_upstream`** | 上游终帧到达 | 由 `terminal_event` 决定 | **不写**（保持 `pending`） | 结算(实际) | 释放 |
| **`finalize_delivery`** | socket write 返回后（经 outbox） | 不写 | **写终态** | 不写 | 不写 |
| **`finalize_abort`** | 请求以取消/断流/超时收尾 | 终态 + `cancel_reason` | **写终态** | 结算 | 释放 |
| **`finalize_recovery`** | 恢复扫描（§4.2bis） | 终态 | **写终态** | 结算 | 释放 |

> 四者共用同一段幂等骨架（reservation 的 `reserved → settled/abandoned` 跃迁是唯一聚合闸门），差别只在**是否写 `requests.final_status`** 与参数。
>
> ⚠️ **无预留的请求必须走旁路**（本轮自查 [critical]）：骨架以"reservation 跃迁成功"为继续执行的闸门，而 **全候选不可用**（无 attempt、无预留）与 **D 阶段预留失败 429** 这两条路径**从未产生 reservation 行** → 闸门恒不通过 → 后续写 `requests.final_status` 的语句也不执行 → **request 永久停留 `pending`**。
> 故 `finalize_abort` 分两种形态：
>
> | 形态 | 判据 | 动作 |
> | --- | --- | --- |
> | **有预留** | 存在 `client_reservations` 行且 `state='reserved'` | 走完整骨架（跃迁 → 扣回聚合 → attempt/canary → 写 request 终态） |
> | **无预留** | 该 `request_id` 无 `client_reservations` 行 | **跳过 ①②④**，只执行 `UPDATE requests SET final_status=:t WHERE id=:rid AND final_status='pending'`；聚合表本就没被动过，无需扣回 |
> **只有 `finalize_upstream` 不写 request 终态**——它跑在字节放行之前，那时交付结果未知；写了就会让 K4 不可达（第 12 轮 [critical]）。其余三者跑在"本请求已无后续"之时，必须写，否则 request 永久 `pending`。

**`finalize_abort` 的 request 终态映射**（`cancel_reason` → `requests.final_status`）：

| `cancel_reason` | request 终态 | 计入 SLA 失败？ |
| --- | --- | --- |
| `client_disconnect` | **`canceled`**（attempt 记 **`canceled_by_client`**） | **否**——用户主动取消，PRD 术语明确不计入 |
| `sla_takeover` | 不终结（本跳取消，请求继续走下一跳）→ 走下方 **`closeout_attempt`** | — |
| `upstream_disconnect` | `failed`（`stream_broken=true` 时计流中断） | 是 |
| `internal_timeout` | `failed` | 是 |
| `upstream_error` | `failed` | 是 |
| 全候选不可用（无 attempt 或全部失败） | `unavailable` | 是 |

**取消时的费用口径**（本轮路径走查发现：`finalize_abort` 只定义了 request 终态，**没说 `actual_usd` 怎么算**——而取消时上游已生成的 token 照样计费，我们却因提前关连接而**永远拿不到终帧 usage**）：

```
actual_usd(被取消的 attempt) =
  ( 已知输入 token 上界 × pv.input_price               -- 与预留同源，len(RawBody) 或 max_input_tokens
  + ceil(旁路已观测到的输出字节数 / 2) × pv.output_price -- 每 token ≥1 字节 → 除 2 是**下界**，
                                                        -- 故乘安全系数 `cancel_cost_safety_ratio`（默认 1.3，[09 §4bis](./09-admin-api.md)）
  ) / u × m × cancel_cost_safety_ratio
      -- pv / u / m 同 §3 成本公式；取消时拿不到 usage，故无缓存分段，全按 input_price 计
```

| 决策 | 理由 |
| --- | --- |
| **不用"单跳保守上界"**（recovery 用的那个） | 用户可能刚发出就取消，按整个上下文窗口预留额记账会**严重高估**，把配额吃光 |
| **不标 `needs_manual_review`** | 客户端取消是**高频**操作，每次都挂人工核对不可行 |
| **旁路字节数是可得的** | `Observation.Bytes` 已在逐事件统计（[03 §2](./03-upstream-layer.md)），无需额外机制 |

> ⚠️ **这是近似值，且是本设计已知的成本误差敞口**（明示，不假装精确）：它既可能高估（安全系数）也可能低估（上游按其自己的分词计费）。因转向自研后**无对账环节**（账本即唯一真相源），该误差**无法被自动纠正**。
> **缓解**：[06 §6](./06-deployment-and-operations.md) 增监控项——`取消请求占比` 与 `取消请求估算成本占总成本比`；后者超阈值（默认 15%）→ **P3**，提示人工抽样核对上游账单并调整 `cancel_cost_safety_ratio`。

> ⚠️ `sla_takeover` 是**唯一不终结 request** 的取消原因：它只结束当前 attempt，请求继续。其余取消原因都意味着"本请求到此为止"，必须走 `finalize_abort`。
> **验收**：[AC-30/AC-32](./14-acceptance-matrix.md) 须断言——客户端断开后 `requests.final_status='canceled'`、`client_reservations.state≠'reserved'`、`client_daily_spend.reserved_usd` 已扣回、canary claim 已释放，且该请求**不计入 SLA 失败**。

**三套 claim 的统一编排**（第 31 轮自查 [critical]：`capacity_claims`／`canary_claims`／`probe_claims` 分别定义在三处，每处都写"与 X 同构"，但**从未合进同一条 dispatch 链**；更要命的是 **capacity 是每个请求都要的**，却完全不在主链里，而释放侧的 `rel` CTE 只放 canary —— 另两套**永不释放，必然泄漏**）：

**申请顺序（固定，不可交换）**：

```text
req      锁 requests(stage='planned', final_status='pending')
 └→ quota    日配额原子预留        （钱）      失败 → 429
 └→ resv     client_reservations
 └→ cap      capacity_claims       （上游容量）失败 → **换候选**，不是 429
 └→ exp      canary_claims 或 probe_claims（实验额度，仅对应 role 才有）失败 → 见下
 └→ att      attempts
 └→ adv      stage='dispatched'
```

**为什么是这个顺序**：钱最贵（拿不到就直接拒），容量次之（可以换个渠道），实验额度最轻（抢不到就退回普通路径）。反过来排会在拿不到钱时白占容量与实验额度。

**四值返回与分支**（`quota_ok` / `capacity_ok` / `exp_ok` / `advanced`）：

| `quota_ok` | `capacity_ok` | `exp_ok` | `advanced` | 处置 |
| --- | --- | --- | --- | --- |
| 0 | — | — | 0 | ROLLBACK → **429**，另起事务写 `stage='reservation_rejected'` |
| 1 | 0 | — | 0 | ROLLBACK → **该 binding 容量已满，换 RoutePlan 下一个候选重试**（不是错误） |
| 1 | 1 | 0 | 0 | ROLLBACK → canary：**回落非 canary 候选**；probe：**本轮跳过该 binding** |
| 1 | 1 | 1 | 1 | COMMIT，发起上游调用 |
| 其余组合 | | | | ROLLBACK + P2 告警（不应出现） |

> `exp_ok` 对普通请求恒为 1（该 CTE 不存在时应用层直接填 1）。

**统一释放**（`finalize_upstream` / `finalize_abort` / `finalize_recovery` / `closeout_attempt` 的 `rel` 段**必须三张表都放**，此前只放了 canary）：

```sql
-- 释放本跳的全部占用；按 attempt_id（recovery 批量时按 request_id）
WITH rel_cap AS (
  UPDATE capacity_claims SET state='released', released_at=now()
   WHERE attempt_id = :attempt_id AND state='active' RETURNING binding_id),
     rel_can AS (
  UPDATE canary_claims   SET state='released', released_at=now()
   WHERE attempt_id = :attempt_id AND state='active' RETURNING binding_id),
     rel_prb AS (
  UPDATE probe_claims    SET state='released', released_at=now()
   WHERE attempt_id = :attempt_id AND state='active' RETURNING binding_id),
     -- 各自的计数扣减（都用 GROUP BY 计数，不可直接 -1）
     cap_agg AS (SELECT binding_id, count(*) n FROM rel_cap GROUP BY binding_id),
     can_agg AS (SELECT binding_id, count(*) n FROM rel_can GROUP BY binding_id)
UPDATE resource_health h
   SET concurrency_inflight = GREATEST(h.concurrency_inflight - COALESCE(c.n,0), 0),
       canary_inflight      = GREATEST(h.canary_inflight      - COALESCE(k.n,0), 0)
  FROM (SELECT binding_id FROM cap_agg UNION SELECT binding_id FROM can_agg) b
  LEFT JOIN cap_agg c ON c.binding_id = b.binding_id
  LEFT JOIN can_agg k ON k.binding_id = b.binding_id
 WHERE h.binding_id = b.binding_id;
-- probe 无 inflight 计数列（其上限靠 probe_budget_windows 的日计数），故只需标 released
```

- **`rpm_used` 不回退**：它是分钟窗口计数，释放并发不等于退还配额。
- **崩溃恢复**：三张 claim 表由**同一个后台任务**回收（[05 §5bis](./05-scheduling-and-operations.md) 任务 7），判据都是 `state='active' AND lease_expires_at < now()`，扣减规则同上。
- **验收**：[AC-08](./14-acceptance-matrix.md)⑩与 [AC-18](./14-acceptance-matrix.md) 须断言——一次 canary 请求结束后，`capacity_claims`、`canary_claims` **两张表**的行都已 `released`，且 `concurrency_inflight` 与 `canary_inflight` **都**归零；只放一张会让另一张的计数永久泄漏。

**接管跳（hop2/hop3…）的插入事务 `dispatch_next`**（第 22 轮 [high]：上面只定义了**首跳**——`attempt_no=1` 且要求 `stage='planned'`。请求进入 `dispatched` 后这条 SQL 再也插不进新 attempt，而 RoutePlan 接管要求**每跳都先落 attempt 再发请求**（[01 §5.1](./01-architecture.md) 意图先行），此前没有任何事务承载它）：

```sql
BEGIN;
WITH req AS (
  SELECT id, created_at FROM requests
   WHERE id = :rid AND created_at = :rcat
     AND stage = 'dispatched' AND final_status = 'pending'   -- 已首跳、单子未关
   FOR UPDATE
),
resv_ok AS (                                   -- 预留仍有效才允许再发一跳
  SELECT 1 FROM client_reservations r, req
   WHERE r.request_id = req.id AND r.state = 'reserved'
),
-- ⚠️ **接管跳同样要占容量**（第 36 轮：此前漏了）——`capacity_claims.kind='takeover'`，
--    天花板 95%（比 normal 高，接管是 SLA 最后防线）。两个 CTE 与首跳同形：
cap_res AS (   -- 已登记容量：条件 UPDATE 原子递增；未登记：SELECT :binding_id FROM resv_ok 直通
  UPDATE resource_health h
     SET rpm_window_start = CASE WHEN h.rpm_window_start IS NULL
                                  OR h.rpm_window_start < date_trunc('minute', now())
                                 THEN date_trunc('minute', now()) ELSE h.rpm_window_start END,
         rpm_used = CASE WHEN h.rpm_window_start IS NULL
                          OR h.rpm_window_start < date_trunc('minute', now())
                         THEN 1 ELSE h.rpm_used + 1 END,
         concurrency_inflight = h.concurrency_inflight + 1
    FROM resv_ok, bindings bd
   WHERE h.binding_id = :binding_id AND bd.id = h.binding_id
     AND (bd.concurrency_limit IS NULL
          OR h.concurrency_inflight < floor(bd.concurrency_limit * :ceiling_takeover))
     AND (bd.rpm_limit IS NULL OR h.rpm_window_start IS NULL
          OR h.rpm_window_start < date_trunc('minute', now())
          OR h.rpm_used < floor(bd.rpm_limit * :ceiling_takeover))
  RETURNING h.binding_id),
cap AS (
  INSERT INTO capacity_claims(claim_id, binding_id, request_id, attempt_id, kind,
                              lease_owner, lease_expires_at)
  SELECT :cap_claim_id, binding_id, :rid, :attempt_id, 'takeover', :owner,
         now() + interval '60 seconds' FROM cap_res
  RETURNING request_id),
-- canary 路径同首跳：此处可挂 canary_claimed / claim_ins 两个 CTE
att AS (
  INSERT INTO attempts(id, request_id, request_created_at, attempt_no, binding_id,
                       single_hop_est_usd, price_version_id, multiplier_version_id, role,
                       attempt_status, lease_owner, lease_heartbeat_at)
  SELECT :attempt_id, req.id, req.created_at, :n, :binding_id,
         :single_hop_est_usd,
         :price_version_id, :multiplier_version_id, :role,   -- 'takeover' | 'retry' | 'canary'
         'pending', :owner, now()
    FROM req, cap                       -- ⚠️ 依赖 cap：容量拿不到就不落 attempt
  RETURNING id
),
-- 首字前接管的**唯一写入点**（第 40 轮：`requests.had_takeover` 建好后全库零赋值，
-- FR-076/092 与 AC-06 都要按它统计接管率，却没有任何路径把它置 true）。
-- ⚠️ 必须 `FROM att`：数据修改型 CTE 无论是否被引用都会执行（本文档反复踩过），
--    不挂在 att 上就会出现"容量没拿到、attempt 没插、却标了接管"。
mark AS (
  UPDATE requests r SET had_takeover = true
    FROM att
   WHERE r.id = :rid AND r.created_at = :rcat
     AND :role = 'takeover'            -- 仅接管跳；retry/canary/probe 不置位
     AND NOT r.had_takeover            -- 幂等：已是 true 就不再写
  RETURNING r.id)
SELECT (SELECT count(*) FROM cap) AS capacity_ok,
       (SELECT count(*) FROM att) AS inserted;
--   capacity_ok=0 → **换 RoutePlan 下一个候选**（不是错误、不是 429）          -- 应用层断言 = 1，否则 ROLLBACK 并放弃该跳
-- ⚠️ 块内不写 COMMIT：同上
```

- **不改 `stage`**：已是 `dispatched`，多跳不推进阶段（[stage 推进协议](#stage-的推进协议)）。
- **不追加预留**：预留在首跳已按 RoutePlan **全部跳**求和（见上方上界算法），接管跳不再动聚合。
- **`attempt_no` 由 `UNIQUE (request_id, request_created_at, attempt_no)` 保证不重复**——并发重复插入会撞唯一约束而失败，正是期望行为。

**首字提交事务 `commit_first_actionable`**（第 28 轮 [P0]：[01 §5.1](./01-architecture.md) 与 [03 §3.0](./03-upstream-layer.md) 都要求"首字先落库再放行字节"，**但从来没有这个事务的 SQL** —— 开发照文档写到这一步就没东西可调）：

```sql
BEGIN;
UPDATE attempts a
   SET attempt_status        = 'committed',
       response_committed_at = now(),
       has_ttft_output       = :has_ttft,              -- 空终态首帧时为 false
       content_aware_ttft_ms = CASE WHEN :has_ttft THEN :ttft_ms ELSE NULL END,
       upstream_first_actionable_at = :arrived_at,      -- 事件**到达**时刻，早于本次提交
       commit_trigger        = :trigger                 -- 'actionable' | 'buffer_limit'（T2 强制提交）
 WHERE a.id = :attempt_id
   AND a.request_id = :rid AND a.request_created_at = :rcat   -- 归属校验
   AND a.attempt_status = 'pending'                            -- 幂等闸门：只提交一次
RETURNING a.id;
COMMIT;
```

| 返回行数 | 含义 | 处置 |
| --- | --- | --- |
| 1 | 首次提交成功 | 放行响应头 + 已缓冲字节 |
| 0 且该 attempt 已是 `committed` | 重复调用（扫描器应保证不发生，防御性） | **视为成功**，照常放行 |
| 0 且 attempt 已终态或不存在 | 参数错配 / 已被恢复扫描收走 | **中断下游流** + P2 告警 |
| 报错 | 库不可用 | **中断下游流**（同 [03 §3.0](./03-upstream-layer.md) 终帧写库失败的语义：无法持久化就不能声称成功） |

- **空终态与首字同帧时**（上游一个 `response.completed` 就结束、中间无 delta）：**先 `commit_first_actionable(has_ttft=false)` 再 `finalize_upstream`**，两个事务顺序执行、不合并。理由：`attempt_status` 的跃迁链 `pending → committed → 终态` 必须完整，否则 [§4.2bis](#42bis-悬挂-attempt-检测补齐-outbox-的盲区) 的分流判据（靠 `response_committed_at` 区分 ② 与 ③）会失效。
- **`commit_trigger` 列**：区分"真见到可执行输出"与"[T2](./15-scope-and-preflight.md) 缓冲上限强制提交"。后者 `has_ttft_output=false`、`content_aware_ttft_ms` 为 NULL，且**不计入 TTFT 统计**——它不是真的首字，只是我们不能再缓冲了。

**事务参数来源表**（第 28 轮 [P0]：`:usage_id`、`:claim_id`、`:event_key` 这些占位符**从来没说过谁生成、怎么生成** —— 而幂等性完全依赖它们。若重试时算出不同的 key，幂等就是假的）：

| 参数 | 生成者 | 规则 | 幂等含义 |
| --- | --- | --- | --- |
| `request_id` | `protocol` 层，C′ 阶段 | **UUIDv7**（时间有序） | 请求的天然主键 |
| `attempt_id` | `selector`/`executor`，每跳 dispatch 前 | **UUIDv7** | 每跳唯一；重试**必须**用新的（一次外部调用=一次上游调用，FR-119） |
| `usage_id` | `ledger`，写 usage 时 | **UUIDv7** | 不承担幂等——幂等由 `UNIQUE (attempt_id, request_created_at, upstream_seq)` 保证 |
| `claim_id` | `selector`，claim 时 | **UUIDv7** | 释放时按它一次性跃迁 |
| **`settle_event_key`** | `ledger` | **稳定组合键**：`<attempt_id>:<terminal_kind>`（如 `018f…:completed`）。**绝不能用随机值** | 同一次结算重试算出**同一个 key** → `UNIQUE` 挡住二次结算 |
| `reservation_adjustments.event_key` | **调用方（运维/管理 API）** 提供 | 建议 `adj:<request_id>:<账单批次号>` | 同一次人工修正重复提交只生效一次 |
| `ledger_outbox.idempotency_key` | `ledger` | `<attempt_id>:<event_type>` | 同一 attempt 的同一事件只投递一次 |

> ⚠️ **`settle_event_key` 是全套幂等的支点**：[§2bis](#2bis-网关调用方凭证域入站鉴权对抗性审查新增) 的三态判定（`applied` / `already_applied_same_event` / `conflict`）靠它区分"幂等重试"与"真冲突"。用随机 UUID 会让每次重试都被判成新结算 → 重复扣费。

**首字提交事务 `commit_first_actionable`** 与下方 `closeout_attempt` 的调用者与线程模型见 [§2quater](#2quater-事务的调用者与线程模型)。

**每跳收尾事务 `closeout_attempt`（`sla_takeover` 专用）**（第 24 轮 [high]）：

> ⚠️ `sla_takeover` 是唯一"结束本跳但不终结 request"的原因，此前**没有任何事务承载它**——hop1 被接管后，它的终态、费用事实、canary claim 释放全都无人负责：
> - hop1 的费用（上游已发出、可能已计费）在 hop2 关单时被漏掉 → `settled_usd` 系统性偏低、日配额可持续被突破；
> - hop1 若是 canary，其 claim 只能等 60s 租约过期回收，而不是随该跳结束立即释放 → 并发额度被白占。

```sql
-- closeout_attempt：只收尾本跳，**不碰 reservation、不写 requests.final_status**
BEGIN;
WITH att AS (
  UPDATE attempts a
     SET attempt_status = 'canceled_by_sla', cancel_reason = 'sla_takeover',
         canceled_by_sla = true, cancel_propagated = :propagated, ended_at = now(),
         -- 第 40 轮补：两列建好后全库零赋值，FR-080/FR-058 因此没有任何数据可依。
         -- `continue_billing`：我们关连接时上游**已经在生成**（已提交首字）——
         --   上游不会因为我们断开就立刻停止计费，故这笔钱**还会继续涨**，
         --   而我们再也观测不到（[§取消时的费用口径](#取消时的费用口径) 的误差敞口就在这里）。
         --   未提交首字就取消的跳不置位：还没开始产出，继续计费的风险可忽略。
         continue_billing  = (a.attempt_status = 'committed'),
         -- `is_duplicate_cost`：本跳的钱照付，但它的输出**没有交付给用户**（被接管取代）。
         --   这正是 FR-058 要度量的"重复请求费用"——接管越多，这个占比越高。
         --   sla_takeover 的每一跳按定义都满足，故恒 true。
         is_duplicate_cost = true
   WHERE a.id = :attempt_id AND a.request_id = :rid
     AND a.request_created_at = :rcat
     AND a.attempt_status IN ('pending','committed')
  RETURNING a.id, a.request_created_at
),
usage_ins AS (                      -- 该跳的费用事实：有真实 usage 用真实，否则按取消口径估算
  INSERT INTO attempt_usage (id, attempt_id, request_created_at, upstream_seq,
                             prompt_tokens, completion_tokens, total_tokens,
                             prompt_cached_tokens, total_cost, estimated_cost,
                             cost_variance, cost_source)
  SELECT :usage_id, att.id, att.request_created_at, 1,
         :prompt, :completion, :total, :cached, :cost, :est, :cost - :est, :cost_source
    FROM att
  ON CONFLICT (attempt_id, request_created_at, upstream_seq) DO NOTHING
  RETURNING id
),
-- ④ 释放本跳的**三套**占用（第 31 轮统一：此前只放 canary，capacity/probe 永不释放必然泄漏）——立即释放，不等租约过期
rel_cap AS (
  UPDATE capacity_claims c SET state='released', released_at=now()
    FROM att WHERE c.attempt_id = att.id AND c.state='active' RETURNING c.binding_id),
rel_can AS (
  UPDATE canary_claims  c SET state='released', released_at=now()
    FROM att WHERE c.attempt_id = att.id AND c.state='active' RETURNING c.binding_id),
rel_prb AS (
  UPDATE probe_claims   c SET state='released', released_at=now()
    FROM att WHERE c.attempt_id = att.id AND c.state='active' RETURNING c.binding_id),
cap_agg AS (SELECT binding_id, count(*) AS n FROM rel_cap GROUP BY binding_id),
can_agg AS (SELECT binding_id, count(*) AS n FROM rel_can GROUP BY binding_id),
dec AS (
  UPDATE resource_health h
     SET concurrency_inflight = GREATEST(h.concurrency_inflight - COALESCE(c.n,0), 0),
         canary_inflight      = GREATEST(h.canary_inflight      - COALESCE(k.n,0), 0)
    FROM (SELECT binding_id FROM cap_agg UNION SELECT binding_id FROM can_agg) b
    LEFT JOIN cap_agg c ON c.binding_id = b.binding_id
    LEFT JOIN can_agg k ON k.binding_id = b.binding_id
   WHERE h.binding_id = b.binding_id
  RETURNING 1)
SELECT count(*) AS closed FROM att;   -- 应用层断言 = 1，否则 ROLLBACK
-- ⚠️ 块内不写 COMMIT：断言在应用层，先提交就回滚不了
```

**接管的执行顺序被冻结为**（第 25 轮 [high]）：

```text
期限到达 → ① Stream.Close() 传播取消到上游（止损优先，AC-32）
        → ② closeout_attempt(hopN)      —— 收尾本跳：终态 + 费用行 + 释放 claim
        → ③ dispatch_next(hopN+1)       —— 插入下一跳 attempt
        → ④ 才发起 hopN+1 的上游调用
```

- **两者不要求同事务**：崩在 ② 与 ③ 之间是安全的——hop1 已收尾、hop2 尚未插入，恢复扫描按最后一跳（此时是 hop1，已终态）走 §4.2bis，`requests.final_status` 仍 `pending` 会被 `finalize_recovery` 关掉。
- **顺序不可颠倒**：若先 `dispatch_next` 再 `closeout_attempt`，崩在中间会留下「hop1 非终态 + hop2 已插入」，恢复的 ⓐ 分支虽能把 hop1 判成 `canceled_by_sla`，但**它的费用行不会被写**（`closeout_attempt` 没跑），Σ 汇总时 hop1 只能按估算计入，精度无谓损失。
- **① 必须最先**：先止损再记账，避免上游继续生成产生额外费用（AC-32 要求取消传播 <1s）。

**`actual_usd(request) = Σ 该 request 全部 attempt` 是所有结算入口的统一规则**（不只恢复路径）：

| 入口 | 结算金额 |
| --- | --- |
| `finalize_upstream`（末跳终帧） | **Σ 全部 attempt**——含此前被 `closeout_attempt` 收尾的接管跳 |
| `finalize_abort` | 同上 |
| `finalize_recovery` | 同上（见 §4.2bis 汇总规则） |
| `closeout_attempt` | **不结算 reservation**，只写本跳费用行 |

> ⚠️ 此前"Σ 全部 attempt"只写在恢复段，正常路径写的是 `actual=真实 usage`。代入 [AC-06](./14-acceptance-matrix.md)：hop1 已发出后被接管取消、hop2 成功 —— hop2 的 `finalize_upstream` 会释放**整笔 RoutePlan 预留**，`actual_usd` 却只取 hop2 的 usage，**hop1 的钱凭空消失**。
>
> **验收**：[AC-06](./14-acceptance-matrix.md)/[AC-08](./14-acceptance-matrix.md) 须断言——正常接管成功后，`actual_usd` 包含 hop1 实际成本、hop1 的 canary claim 已 `released` 且 `canary_inflight` 已扣减（**不依赖租约过期**）。

**关单必须两阶段：`finalize_upstream` → `finalize_delivery`**（第 12 轮 [critical]）：

> ⚠️ 上一版让 `finalize` 在**终帧到达时**就把 `requests.final_status` 写成 `completed`。但那一刻终帧**还没写给下游**（[03 §3.0](./03-upstream-layer.md) 要求先落库再放行）、`downstream_write_completed_at` 也还不存在。于是随后落入 K4 窗口崩溃时，request **早已是终态**，恢复 SQL 的 `WHERE final_status='pending'` 闸门再也改不动它 —— **第 11 轮新增的 K4 语义被正常关单路径整个绕过**，等于白加。

| 阶段 | 触发 | 写什么 | 同步性 |
| --- | --- | --- | --- |
| **`finalize_upstream`** | 上游**终帧到达**（成本此刻已确定） | reservation 结算（上方 SQL）+ `attempt_status` 终态 + `terminal_event`/usage | **同步**，且必须在放行终帧字节**之前**提交 |
| **`finalize_delivery`** | 下游 **socket write 返回之后** | `downstream_write_completed_at` + `requests.final_status` | 异步经 outbox（不涉及计费） |

```sql
-- finalize_delivery（两步，同一事务；由 outbox 事件 downstream_write_completed 驱动）
BEGIN;
-- ① 幂等写入写出事实（重放时 IS NULL 条件不再成立 → 0 行，不覆盖更早的真实时刻）
UPDATE attempts
   SET downstream_write_completed_at = :written_at
 WHERE id = :attempt_id AND downstream_write_completed_at IS NULL;
-- ② 据该事实推 request 终态（幂等：只在 pending 时推一次）
--    ⚠️ request_id / request_created_at **从 attempts 派生**，不由 payload 传（第 17 轮 [high]）：
--       outbox 的 payload 只含 attempt_id/written_at，直接用 :rid 会依赖一次未说明的额外查询。
-- ⚠️ 第 23 轮 [high]：条件不能只写 `final_status='pending'`。recovery 可能**先一步**把
--    该 request 关成保守的 `interrupted`（K4 分支），迟到的 delivery 若只补写时间戳而不改终态，
--    就会留下"写完事实存在、request 却是 interrupted"的错误 SLA 结论。
--    真实交付事实**有权纠正**保守推定，但只允许 `interrupted → completed/failed` 这一个方向，
--    不得把已确定的 completed/failed/canceled 改回去。
UPDATE requests r
   SET final_status = CASE
         WHEN a.downstream_write_completed_at IS NOT NULL
              AND a.attempt_status = 'completed'                THEN 'completed'
         WHEN a.attempt_status IN ('failed','unknown_billing')  THEN 'failed'
         ELSE 'interrupted'                                      -- 写出未确认 → 保守
       END,
       -- ③ 对外有效首字（第 40 轮补：该列建好后全库零赋值，而 AC-06「最终 TTFT < 等级目标」
       --    与 AC-31 的对外口径全靠它 —— 没有计算路径，验收无处可断言）。
       --    取**被提交那一跳**的自算 TTFT：被 SLA 取消的前几跳不算数（用户没看到它们的字）。
       --    `has_ttft_output=false`（空终态）时保持 NULL —— 没有首字就没有首字延迟。
       effective_ttft_ms = CASE WHEN a.has_ttft_output THEN a.content_aware_ttft_ms
                                ELSE r.effective_ttft_ms END
  FROM attempts a
 WHERE a.id = :attempt_id
   AND r.id = a.request_id
   AND r.created_at = a.request_created_at                       -- 分区键对齐
   AND r.final_status IN ('pending','interrupted');              -- 允许纠正保守终态

-- ④ 连续会话前缀账目（FR-050/051；AC-06/07）
-- ⚠️ 第 40 轮：`session_prefix_ledger` **全库没有任何写入语句**，而 §1.3 动态期限算法
--    读它的 `cumulative_first_token_ms` —— 悬空读，期限会恒等于默认值，FR-051 无从判定。
INSERT INTO session_prefix_ledger(
         session_id, turn_no, request_id,
         turn_first_token_ms, cumulative_first_token_ms, prefix_avg_ms, target_ms, met_target)
SELECT r.session_id,
       COALESCE(prev.turn_no, 0) + 1,                            -- 轮次由本表自增，调用方不传
       r.id,
       r.effective_ttft_ms,
       COALESCE(prev.cumulative_first_token_ms, 0) + COALESCE(r.effective_ttft_ms, 0),
       (COALESCE(prev.cumulative_first_token_ms, 0) + COALESCE(r.effective_ttft_ms, 0))
         / (COALESCE(prev.turn_no, 0) + 1),                      -- 前缀平均
       :prefix_target_ms,                                        -- config_params['prefix_target_ms']
       (COALESCE(prev.cumulative_first_token_ms, 0) + COALESCE(r.effective_ttft_ms, 0))
         / (COALESCE(prev.turn_no, 0) + 1) <= :prefix_target_ms
  FROM attempts a                                                -- ⚠️ 与 ② 同源：从 attempts 派生
  JOIN requests r ON r.id = a.request_id AND r.created_at = a.request_created_at
  LEFT JOIN LATERAL (
       SELECT turn_no, cumulative_first_token_ms FROM session_prefix_ledger
        WHERE session_id = r.session_id ORDER BY turn_no DESC LIMIT 1) prev ON true
 WHERE a.id = :attempt_id
   AND r.session_id IS NOT NULL                                  -- 无会话键的请求不入账
   AND r.effective_ttft_ms IS NOT NULL                           -- 没首字不构成一轮
ON CONFLICT (session_id, turn_no) DO NOTHING;                    -- 见下方并发说明
COMMIT;
```

- **只用 `:attempt_id` 一个参数**：outbox payload 只有 `attempt_id`/`written_at`。
  ④ 与 ② 一样**从 attempts 派生** `request_id`/`request_created_at`，
  **不得**写成 `:rid`/`:rcat`——那会依赖一次本事务没有说明来源的额外查询（第 17 轮 [high] 定的规矩）。
- **④ 读得到 ③ 刚写的 `effective_ttft_ms`**：同一事务内后一条语句可见前一条的效果。
- **轮次并发**：同一 `session_id` 的两个请求并发关单会算出相同 `turn_no` → 撞主键。
  `ON CONFLICT DO NOTHING` 让后到者**丢弃本轮记账**而不是报错——连续会话本就是串行对话，
  并发同会话属异常输入；丢一轮记账的代价远小于让关单事务失败。
  ⚠️ 但**不要**改成 `DO UPDATE`：那会把两轮的首字混算进同一行，前缀平均直接失真。
- **`met_target` 是每轮结论，不回溯**：某轮不达标不会改写既往行（FR-051 要的是逐轮判定，不是终局判定）。

**首字节的写出事实同理**：`downstream_first_byte` 事件写 `attempts.downstream_first_byte_written_at`（同样 `WHERE ... IS NULL` 幂等），它不推 request 终态，只用于恢复扫描区分 ③b1／③b2 与统计 `stream_break_rate`。

**两个事件的 `payload`**：`{"attempt_id": <uuid>, "written_at": <timestamptz>}`；`idempotency_key` = `<attempt_id>:downstream_first_byte` / `<attempt_id>:downstream_write_completed`。

- **计费与交付彻底解耦**：钱在 `finalize_upstream` 就结清（成本那时已知），交付结论晚一步不影响配额正确性。
- **崩溃在两阶段之间** = 正是 K4：reservation 已 settled、attempt 已终态、request 仍 `pending` → 恢复扫描按 §4.2bis ③c 判 `interrupted`。**这条路径现在真的可达了。**

**调用路径（都从 `reserved` 出发），共用同一幂等骨架，参数不同**：

| 路径 | 触发 | `outcome` → reservation / attempt / **request** |
| --- | --- | --- |
| **`finalize_upstream`（正常路径）** | 终帧到达 | `settled`(actual=真实 usage) / 由 `terminal_event` 决定 / **保持 `pending`** |
| **`finalize_abort`** | 客户端断开／上游断流／内部超时／全候选不可用 | 见上方 `cancel_reason` → `final_status` 映射表 |
| **`finalize_recovery`**（§4.2bis） | 租约超时 | 见 §4.2bis 分流表（五种 outcome）；**必须显式写 `requests.final_status`** |

> ⚠️ **正常路径也不写 `requests.final_status`**（第 16 轮 [critical]）：上一版这一行写成 `completed / completed`，等于终帧一到达就把 request 关成成功——那正是第 12 轮修掉的老毛病，会让 K4 窗口的崩溃被记成成功。request 终态**只能**由 `finalize_delivery` 依据 `downstream_write_completed_at` 推定。

**人工核对走独立的 adjustment 事务**（第 8 轮 [critical]）：

> ⚠️ 上一版把人工核对也塞进 `finalize`，**永远不可能生效**——`finalize` 的幂等闸门是 `WHERE state='reserved'`，而恢复扫描早已把待核对行置为 `settled`，第一条 UPDATE 必然影响 0 行、事务直接返回。结果：错误的保守估算**永久留在** `settled_usd` 里，人工核对形同虚设。
>
> 故人工修正必须是**从 `settled` 出发的增量修正**，走另一套幂等键。

```sql
-- 修正事件表：幂等键独立于 settle_event_key
CREATE TABLE reservation_adjustments (
  event_key     TEXT PRIMARY KEY,              -- 幂等键：重复提交/崩溃重放只生效一次
  request_id    UUID NOT NULL REFERENCES client_reservations(request_id),
  old_actual_usd nonneg_usd NOT NULL,          -- 修正前值（审计）
  new_actual_usd nonneg_usd NOT NULL,
  operator      TEXT NOT NULL,                 -- 责任人（FR-099/104）
  reason        TEXT NOT NULL,                 -- 依据（如"上游账单 #12345"）
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

```sql
-- adjust(request_id, new_actual_usd, event_key, operator, reason)
BEGIN;
-- (1) 先锁住目标 reservation，并从**锁定行**派生 client、日期与旧值
--     第 9 轮 critical：上一版直接接受 :cid/:date 参数且不加锁 →
--       a) 两个不同 event_key 的并发修正会读到同一个旧值，各自把 (new-old) 加进聚合，
--          旧值 10、并发改 12 和 13 → 聚合变 15，而 reservation 只能是 12 或 13，永久失真；
--       b) 参数填错可以改到**别的客户或别的日期**的聚合值。
-- ⚠️ 第 37 轮：`SELECT ... INTO :var` 是 PL/pgSQL 语法，**普通客户端连接跑不了**。
--    改为 CTE 派生：locked 既加行锁，又把 client/日期/旧值传给后续语句。
WITH locked AS (
  SELECT gateway_client_id, spend_date, actual_usd
    FROM client_reservations
   WHERE request_id = :rid AND state = 'settled'
     FOR UPDATE                                  -- 空结果 → 后续全不执行（等价于回滚）
),
-- (2)(3) 幂等闸门与后续更新**必须绑定在同一条语句里**（第 17 轮 [high]）：
--     原版把 gating 只写在注释（"仅当影响行数=1 才执行"），SQL 本体没表达 →
--     同一 event_key 携带**不同** new_actual_usd 重试时，INSERT 被 DO NOTHING 吃掉，
--     后面两个 UPDATE 却照跑，聚合与 reservation 被二次改写。
-- ⚠️ 第 38 轮：此处原是第二个 `WITH`（第 37 轮加 locked 时留下的），一条语句里
--    两个 WITH 不可执行。已并入上面同一条 CTE 链。
inserted AS (
  INSERT INTO reservation_adjustments(event_key, request_id, old_actual_usd, new_actual_usd, operator, reason)
  SELECT :event_key, :rid, l.actual_usd, :new_actual, :operator, :reason FROM locked l
  ON CONFLICT (event_key) DO NOTHING
  RETURNING request_id, old_actual_usd, new_actual_usd
),
agg AS (
  UPDATE client_daily_spend d
     SET settled_usd = d.settled_usd + (i.new_actual_usd - i.old_actual_usd)  -- 差额
    FROM inserted i
   WHERE d.gateway_client_id = (SELECT gateway_client_id FROM locked)
     AND d.spend_date        = (SELECT spend_date FROM locked)                -- 均派生自锁定行
  RETURNING 1
)
UPDATE client_reservations r
   SET actual_usd = i.new_actual_usd, needs_manual_review = false
  FROM inserted i
 WHERE r.request_id = i.request_id;
-- inserted 为空（重放）→ agg 与最后一句都影响 0 行，聚合与 reservation 均不动
COMMIT;
```

- **同 `event_key` 携带不同金额的重试**：按上面的 CTE 语义**静默无操作**（第一次的值为准）。若希望暴露调用方错误，API 层可在 `ON CONFLICT` 命中且 `new_actual_usd` 与已存值不一致时返回 **409**——但**数据库层的正确性不依赖它**。
- **`new_actual_usd` 必须 ≥ 0**：该列用 `nonneg_usd` 域（带 `CHECK (VALUE >= 0)`），API 层另行返回 **400** 拒绝负值（不依赖数据库报错）。修正后 `settled_usd` 亦须 ≥ 0，否则整事务回滚。
- **差额修正而非覆盖**：`settled_usd += (new − old)`，配合 `event_key` 幂等键，重复提交与崩溃重放都只生效一次。
- **`cid` / `spend_date` / `old_actual` 三者全部从 `FOR UPDATE` 锁定的 reservation 行派生**，不接受调用方传参——既挡住并发丢失更新，也挡住改错客户、改错日期。跨日核对因此天然记到原始那一天。
- **禁止直接改 `client_daily_spend`**：所有聚合修改只有 `finalize` 与 `adjust` 两个入口。
- **验收**：见 [AC-33](./14-acceptance-matrix.md) ⑭／⑮／⑱／⑲（成功修正 / 重复 event_key 与崩溃重放 / **两个不同 event_key 并发修正后聚合与 reservation 必须一致** / 参数不可指定 client 与日期）。

> **验收**：[AC-33](./14-acceptance-matrix.md) 须含**事务幂等重试**测试——结算已不经 outbox（见上方写入路径分工），故场景改为：同一 `settle_event_key` 重复执行 `finalize_upstream`、或事务提交后连接中断导致调用方重试，`settled_usd` **不得翻倍**、`reserved_usd` **不得为负**。[AC-35](./14-acceptance-matrix.md) 须在**四个崩溃时点各断言** `client_reservations.state ≠ 'reserved'` 且 `client_daily_spend.reserved_usd` 已归零（无泄漏）。

**`stage` 的推进协议**（第 19 轮 [critical]：上一版只定义了枚举与恢复判据，**没有任何 `SET stage` 的时机**，开发无从实现；且恢复分流漏了 `planned`——崩在 selector 之后、预留之前同样是零 attempt，却没有对应分支）：

| 时机 | 写入 | 同事务对象 |
| --- | --- | --- |
| C′ 落 `requests` | `stage='authenticated'`（DEFAULT） | 单独事务 |
| selector 输出**空**候选集 | `stage='no_candidates'` | 与写 `decision_snapshot`（含排除原因）同事务 |
| selector 产出**非空** RoutePlan | `stage='planned'` | 与写 `decision_snapshot` 同事务 |
| D 阶段原子预留返回 0 行 | `stage='reservation_rejected'` | 单独事务（预留事务已回滚） |
| 首个 attempt 落库 | `stage='dispatched'` | **与 `client_reservations` 插入、`attempts` 插入同事务**（D 阶段那一个事务） |

**单调推进**：`authenticated → {no_candidates | planned → {reservation_rejected | dispatched}}`，只进不退。多跳接管**不改变** `stage`（已是 `dispatched`）。

> ⚠️ **`stage` 只用于零 attempt 请求的归因**，不参与任何调度决策。有 attempt 的请求一律走 §4.2bis 的 ①②③ 分支，不看 `stage`——避免同一事实两个来源。

恢复分流的 ⓪ 分支据此补全为四种（见 §4.2bis）。

**鉴权拒绝的审计载体**（本轮路径走查发现：FR-120 明写"支持…**使用审计**"，但唯一载体 `gateway_clients.last_used_at` 只记**成功使用**；被拒绝的请求既不落 `requests`（C′ 在鉴权**之后**），也没有别的地方记 → 一把已吊销的 Key 被反复打、或某调用方持续探测越权别名，**系统完全看不见**）：

```sql
-- 鉴权拒绝的**聚合**审计：按分钟窗口计数，不逐请求落行
CREATE TABLE auth_rejections (
  window_start  TIMESTAMPTZ NOT NULL,          -- 分钟粒度对齐
  gateway_client_id BIGINT REFERENCES gateway_clients(id),  -- 401(无凭证/无法定位) 时为 NULL
  secret_prefix TEXT,                          -- 可定位但校验失败时记前缀（**永不记完整凭证**，FR-094）
  reason        TEXT NOT NULL CHECK (reason IN
                  ('no_credential','bad_secret','revoked','expired','alias_forbidden')),
  count         INTEGER NOT NULL DEFAULT 0,
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  -- ⚠️ 不可把可空列放进 PRIMARY KEY（第 18 轮 [critical]）：PostgreSQL 的主键列强制 NOT NULL，
  --    而**匿名 401**（无凭证、无法定位前缀）恰恰两列都为 NULL → 审计行根本写不进去，AC-33 必败。
  --    改用代理主键 + NULLS NOT DISTINCT 唯一索引实现"每分钟每组合一行"的聚合语义。
  UNIQUE NULLS NOT DISTINCT (window_start, gateway_client_id, secret_prefix, reason)
);
```

> ⚠️ **为什么是计数而不是逐请求落 `requests` 行**：401 发生在鉴权**之前**，意味着**任何人**都能触发它。若逐请求写库，未鉴权流量直接变成**写库放大**——一个脚本就能把账本表打满、把 PG 拖垮，等于给了攻击者一条比业务请求更廉价的攻击路径。按分钟窗口聚合后，无论打多少次，每分钟每（凭证前缀 × 原因）最多一行。
>
> **明确结论：401/403 不进 `requests` 账本。** 这是刻意选择，不是遗漏——它们从未进入调度，没有候选、没有价格版本、没有 attempt，FR-097/098 要求记录的字段一个都不存在。它们的审计需求由本表满足。
>
> **告警**：单窗口 `revoked`/`bad_secret` 计数超阈值 → **P2**（疑似凭证泄露或被爆破）；`alias_forbidden` 持续出现 → P3（调用方配置错误）。阈值走 `config_params`。
>
> **服务 FR/AC**：FR-120（使用审计）、FR-094（不展示完整凭证）；[AC-33](./14-acceptance-matrix.md) 须断言 401/403 **不产生 `requests` 行**、但 `auth_rejections` 计数递增。

**内置探测凭证 `system-probe`**（主动测活需要一个记账主体——`requests.gateway_client_id` 是 NOT NULL，而探测请求不属于任何真实调用方）：

```sql
-- 迁移种子创建，不经 /admin/clients
INSERT INTO gateway_clients (name, secret_hash, secret_prefix, status, is_system,
                             allowed_aliases, quota_daily_usd, rpm_limit,
                             tenant_id, region, business_tier, data_class)
VALUES ('system-probe',
        '<不可用的哨兵值>', 'sys-probe',      -- 无有效明文，外部永远无法通过鉴权
        'system_internal', true,
        NULL,                                  -- ⚠️ 见下：别名范围
        NULL,                                  -- 不占任何日配额（由 probe_budget_windows 约束）
        60,                                    -- ⚠️ 见下：必须设 RPM
        'system', '*', 'internal', 'internal');
```

**三项容易漏掉的属性**（缺任一都会出问题）：

| 属性 | 取值 | 不这么设会怎样 |
| --- | --- | --- |
| `allowed_aliases` | **NULL（全部别名）**，但探测目标由 `probe_templates.model_id` 决定 | 若按普通凭证理解成"限定几个别名"，新增模型时探测会静默失效；设 NULL 是因为**探测的准入由测活资格与预算控制，不由别名白名单控制** |
| `rpm_limit` | **必须设**（默认 60），不可留 NULL | NULL = 不限速（[§2bis](#2bis-网关调用方凭证域入站鉴权对抗性审查新增) B 阶段整段跳过）→ 探测风暴无上限，五维预算只管钱不管速率 |
| 四维数据许可属性 | `tenant='system'`、`data_class='internal'` 等**显式填写** | 全 NULL → 求值时按 `'*'` 处理；二期开启 `data_policy_enabled` 后，探测可能被**我们自己的规则**挡掉，且排障时看不出原因 |

- **鉴权层硬拒**：`status='system_internal'` 的凭证，`/v1/*` 入口**直接 401**，不做哈希比对（它的 `secret_hash` 本就是哨兵值）。
- **管理接口保护**：`/admin/clients/{id}/revoke`、`/rotate`、删除操作遇 `is_system=true` 一律 **403**（[09](./09-admin-api.md)）。
- **报表单独归因**：探测消费在成本报表中按 `requests.probe_kind='probe'` 单列，**不混入业务口径**（FR-072 的"计入实际成功成本"仍成立——它进总成本，只是分开展示）。

**鉴权与预留的执行顺序**（第 9 轮 [critical] 修正）：

> ⚠️ 上一版把"校验 `quota_daily_usd`"写在鉴权阶段（selector 之前），但 `estimated_usd` **必须等 selector 产出完整 RoutePlan 才算得出来**——顺序上不可能在那时校验。照原文实现必然写成 check-then-act：多实例同时通过基于旧余额的检查，再各自预留，配额直接被突破（违反 AC-33⑥⑬）。故拆成**快速拒绝**与**原子预留**两段。

| 阶段 | 位置 | 动作 |
| --- | --- | --- |
| **A. 鉴权** | `protocol` 层，最先 | ① `Authorization: Bearer <token>` → 按 `secret_prefix` 定位 → 校验 `secret_hash`；② `status='active'` 且未过期，否则 **401**；③ 模型别名 ∈ `allowed_aliases`，否则 **403** |
| **B. RPM 原子闸** | 同上，鉴权后 | 见下方原子语句；返回 0 行即 **429**。RPM 不依赖 RoutePlan，可在此完成 |
| **C. 日配额快速拒绝** | 同上 | 读内存快照：若 `settled_usd + reserved_usd ≥ quota_daily_usd` 直接 **429**。**这是优化不是保证**——只为省掉必然失败的调度开销，正确性由 D 承担 |
| **C′. 落 `requests` 行** | 鉴权通过后、进入 policy/selector 前 | **同步插入** `requests`（`final_status='pending'`）。**必须早于 attempt 与预留**——否则零 attempt 路径（全候选不可用、D 失败 429）不落账，违反 FR-097/098 与 AC-15 |
| **D. 原子预留** | **selector 产出 RoutePlan 之后、发起上游调用之前** | 算 `estimated_usd` → 见下方原子语句；返回 0 行即 **429**。**这是日配额的唯一正确性保证点** |
| **E. 进入执行** | — | `requests.tenant_id`/`region`/`business_tier`/`data_class` 全部取自该凭证行（§2ter） |

```sql
-- B. RPM 跨实例原子递增 + 判定（一条语句，无 check-then-act）
INSERT INTO client_rate_window(gateway_client_id, window_start, request_count)
VALUES (:cid, date_trunc('minute', now()), 1)
ON CONFLICT (gateway_client_id, window_start) DO UPDATE
   SET request_count = client_rate_window.request_count + 1
 WHERE client_rate_window.request_count < :rpm_limit      -- 已达上限则 DO UPDATE 不执行
RETURNING request_count;
-- 返回 0 行 → 429
-- ⚠️ **`rpm_limit IS NULL`（不限速）时不得执行本语句**（第 17 轮 [high]）：
--    SQL 三值逻辑下 `count < NULL` 恒为 UNKNOWN，DO UPDATE 永不执行，
--    行已存在后每次都返回 0 行 → 一个「不限速」的凭证从第二个请求起全部 429。
--    正确做法：应用层判 rpm_limit IS NULL 则**整段跳过 B 阶段**（连计数也不必写）。
```

```sql
-- D. 日费用原子预留：限额判断 + reserved_usd 递增 + reservation 插入，同一事务
BEGIN;
INSERT INTO client_daily_spend(gateway_client_id, spend_date)
VALUES (:cid, :date) ON CONFLICT DO NOTHING;              -- 保证当日行存在

-- ⚠️ 第 21 轮 [critical]：CTE 里的数据修改语句**无论主语句是否影响行都会执行**。
--    上一版把 `UPDATE requests SET stage='dispatched'` 放在主语句且不校验行数 →
--    参数错配、请求已终态、created_at 不匹配时，reservation 与 attempt **已经写进去了**，
--    stage 却停在 planned → 恢复扫描按零…不，是按「有 attempt」走 ①②③ 分支，
--    而 stage 与实际状态不符，且没有任何一步会回滚那笔预留。
-- 修法：① 先 FOR UPDATE 锁住 requests 并把它作为整条链的输入；
--       ② 末尾必须 RETURNING，由应用层断言恰好 1 行，否则 ROLLBACK。
WITH req AS (
  SELECT id, created_at FROM requests
   WHERE id = :rid AND created_at = :rcat
     AND stage = 'planned' AND final_status = 'pending'
   FOR UPDATE                                             -- 锁住，防并发终态抢先
),
claimed AS (
  UPDATE client_daily_spend d
     SET reserved_usd = d.reserved_usd + :est
    FROM req                                              -- req 为空 → 本句不执行
   WHERE d.gateway_client_id = :cid AND d.spend_date = :date
     AND d.reserved_usd + d.settled_usd + :est <= :quota   -- 判断与递增同一条 UPDATE
  RETURNING d.gateway_client_id
),
resv AS (
  INSERT INTO client_reservations(request_id, gateway_client_id, spend_date, estimated_usd, state)
  SELECT :rid, :cid, :date, :est, 'reserved' FROM claimed
  RETURNING request_id
),
-- ⚠️ capacity 占用：**每个请求都要**（统一编排）。第 33 轮修正两处：
--    ① 原版只 INSERT claim、**没有原子递增计数** → EXISTS 是读判定，并发下容量闸无效；
--       必须用条件 UPDATE ... RETURNING 递增，才具备原子性。
--    ② 原版说"未登记容量则跳过本 CTE"，但下面的 att 固定 FROM cap → 跳过就断链。
--       改为**两个变体**，由应用层按是否登记容量二选一（结构同形，att 无需改）。
cap_res AS (          -- 【变体 A：已登记容量】条件 UPDATE 原子递增
  UPDATE resource_health h
     SET rpm_window_start = CASE WHEN h.rpm_window_start IS NULL
                                  OR h.rpm_window_start < date_trunc('minute', now())
                                 THEN date_trunc('minute', now()) ELSE h.rpm_window_start END,
         rpm_used = CASE WHEN h.rpm_window_start IS NULL
                          OR h.rpm_window_start < date_trunc('minute', now())
                         THEN 1 ELSE h.rpm_used + 1 END,
         concurrency_inflight = h.concurrency_inflight + 1
    FROM resv, bindings bd
   WHERE h.binding_id = :binding_id AND bd.id = h.binding_id
     -- ⚠️ 两列**各自可能单独为 NULL**（只登记了 RPM 或只登记了并发），
     --    且 rpm_window_start 首次为 NULL —— 三种情形都必须显式放行，
     --    否则「只登记并发」的 binding 会因 rpm 条件恒 UNKNOWN 而永远拿不到容量。
     AND (bd.concurrency_limit IS NULL
          OR h.concurrency_inflight < floor(bd.concurrency_limit * :ceiling))
     AND (bd.rpm_limit IS NULL
          OR h.rpm_window_start IS NULL
          OR h.rpm_window_start < date_trunc('minute', now())
          OR h.rpm_used < floor(bd.rpm_limit * :ceiling))
  RETURNING h.binding_id
),
-- 【变体 B：未登记容量（bd.rpm_limit 与 bd.concurrency_limit 皆空）】
--   把上面 cap_res 那一段整体换成直通：
--     cap_res AS (SELECT :binding_id AS binding_id FROM resv)
--   —— 不碰 resource_health，请求路径不多一次同步写（保 P99≤50ms）
-- ⚠️ **变体 B 仍然写 capacity_claims，下面的 cap 段不变**（第 42 轮明确：
--    05 §1.1 序 7 写的"未登记容量的渠道不设保留、本层直接放行"读起来像"跳过整段"，
--    与这里的结构冲突，开发只能靠猜）。理由：claim 不只是计数器，
--    它还是**租约与悬挂回收的载体**（[§6bis](#6bis-claim-回收) 的回收任务、
--    [§4.2bis](#42bis-崩溃恢复扫描) 的悬挂检测都按 claim 找孤儿 attempt）。
--    不写 claim = 未登记容量的 binding 上的请求崩溃后无人回收。
--    "不设保留"指的是**不做闸门判定**（没有上限可判），不是"不留痕迹"。
-- ⚠️ 释放侧对称：`rel_cap` 照常把 claim 置 released；`dec` 递减 resource_health 时
--    对未登记 binding 而言计数本来就是 0，`GREATEST(...,0)` 使其保持 0，不会变负。
cap AS (
  INSERT INTO capacity_claims(claim_id, binding_id, request_id, attempt_id, kind,
                              lease_owner, lease_expires_at)
  SELECT :cap_claim_id, binding_id, :rid, :attempt_id, :cap_kind, :owner,
         now() + interval '60 seconds'
    FROM cap_res
  RETURNING request_id
),
att AS (
  INSERT INTO attempts(id, request_id, request_created_at, attempt_no, binding_id,
                       single_hop_est_usd, price_version_id, multiplier_version_id, role,
                       attempt_status, lease_owner, lease_heartbeat_at)
  SELECT :attempt_id, :rid, :rcat, 1, :binding_id,
         :single_hop_est_usd,                    -- 取自 RoutePlanEntry.SingleHopUSD
         :price_version_id, :multiplier_version_id, :role,
         'pending', :owner, now() FROM cap        -- ⚠️ 依赖 cap，容量拿不到就不落 attempt
  RETURNING request_id
),
adv AS (
  UPDATE requests r SET stage = 'dispatched'
    FROM att
   WHERE r.id = att.request_id AND r.created_at = :rcat
  RETURNING r.id
)
-- ⚠️ 必须**分别**返回三类结果（第 22 轮 [high]）：canary 抢不到**不是错误**
--    （[05 §2.0](./05-scheduling-and-operations.md)：抢不到是常态，应回落到正常排序），
--    与"配额不足"混为一谈会在 canary 并发竞争时**拒绝本可服务的请求**。
SELECT (SELECT count(*) FROM claimed) AS quota_ok,
       (SELECT count(*) FROM cap)     AS capacity_ok,
       (SELECT count(*) FROM adv)     AS advanced;   -- 普通路径 exp_ok 恒填 1
-- ⚠️ **块内不写 COMMIT**：分支表要求 `exp_ok=0` / `capacity_ok=0` 时 ROLLBACK，
--    若 SQL 先 COMMIT，已递增的 `reserved_usd` 与容量计数就被提交下去了。
--    提交与否**一律由应用层依四值分支决定**（见上方统一编排）。
-- ⚠️ 上式是**普通路径**：普通请求没有 canary/probe 的 CTE，无条件读取会语法错。
--    完整顺序见上方「三套 claim 的统一编排」：req→quota→resv→cap_res→cap→exp→att→adv。
```

**应用层据四值分支**（与上方[统一编排](#三套-claim-的统一编排)同一张表，此处不重复列——以那一节为准）：
`quota_ok=0` → 429；`capacity_ok=0` → **换候选**；`exp_ok=0` → canary 回落普通路径 / probe 跳过；四值全 1 → 提交。

**canary 路径的两个 CTE 片段**（⚠️ **这不是一段完整可执行 SQL** —— 它只给出 canary 专有的两个 CTE，`req`/`claimed`/`resv`/`cap_res`/`cap`/`att`/`adv` 与普通路径完全相同，见上方主链。完整链固定为 `req→quota→resv→cap_res→cap→canary_claimed→claim_ins→att→adv`。本段是**上方[统一编排](#三套-claim-的统一编排)的一个实例**，不是并行的另一套真相源。统一编排定义**顺序与四值分支**，本段给出 canary 那两个 CTE 的具体 SQL；二者冲突时**以统一编排为准**）：在 `resv` 与 `att` 之间插入两个 CTE，`att` 改为 `FROM claim_ins`，末尾返回四值：

```sql
-- ...（req / claimed / resv 三个 CTE 与普通路径完全相同）...
-- ⚠️ 第 34 轮修正：canary 链此前 canary_claimed 依赖 resv 而非 cap →
--    容量已满时仍会抢到 canary 名额并落 attempt。固定链为
--    req → quota → resv → cap_res → cap → canary_claimed → claim_ins → att → adv，
--    返回 quota_ok / capacity_ok / exp_ok / advanced **四值**。
-- （cap_res / cap 两段与普通路径完全相同，此处不重复；见上方主链）
canary_claimed AS (          -- resource_health 条件 UPDATE：窗口翻转 + 次数 + inflight
  UPDATE resource_health h
     SET canary_window_start = CASE WHEN h.canary_window_start IS NULL
                                     OR h.canary_window_start < date_trunc('hour', now())
                                    THEN date_trunc('hour', now()) ELSE h.canary_window_start END,
         canary_used_in_window = CASE WHEN h.canary_window_start IS NULL
                                       OR h.canary_window_start < date_trunc('hour', now())
                                      THEN 1 ELSE h.canary_used_in_window + 1 END,
         canary_inflight = h.canary_inflight + 1
    FROM cap                       -- ⚠️ 依赖 cap，不是 resv：容量拿不到就不该抢 canary 名额
   WHERE h.binding_id = :binding_id AND h.health_state = 'canary'
     AND h.canary_inflight < :max_concurrent
     AND (h.canary_window_start IS NULL
          OR h.canary_window_start < date_trunc('hour', now())
          OR h.canary_used_in_window < :max_per_hour)
  RETURNING h.binding_id
),
claim_ins AS (
  INSERT INTO canary_claims(claim_id, binding_id, request_id, attempt_id,
                            lease_owner, lease_expires_at)
  SELECT :claim_id, binding_id, :rid, :attempt_id, :owner, now() + interval '60 seconds'
    FROM canary_claimed
  RETURNING request_id
)
-- att 改为 FROM claim_ins（而非 FROM resv），其余同普通路径
SELECT (SELECT count(*) FROM claimed)        AS quota_ok,
       (SELECT count(*) FROM cap)            AS capacity_ok,
       (SELECT count(*) FROM canary_claimed) AS exp_ok,
       (SELECT count(*) FROM adv)            AS advanced;
-- 同样**不在块内 COMMIT**；四值分支见上方统一编排表
```

> **为什么必须同事务**（第 21 轮 [high]）：[05 §2.0](./05-scheduling-and-operations.md) 要求 claim 与 attempt 同事务；分开写则崩在中间会泄漏 `canary_inflight`。
> **验收**：AC-08⑩ 与 AC-33 须断言五者（预留/claim/inflight/attempt/stage）同事务成败。


- **`quota_daily_usd IS NULL`（不限额）的分支**（第 28 轮 [P1]：原文只说"跳过限额判断"，但上面的 SQL 硬依赖 `<= :quota` 条件，`:quota` 传 NULL 会让整个条件恒为 UNKNOWN → `claimed` 永远 0 行 → **不限额的凭证一个请求都发不出去**）。
  实现上是**同一条 SQL 的两个变体**，由应用层按 `rpm_limit IS NULL` 同样的方式二选一：
  ```text
  不限额变体：claimed 的 WHERE 只保留
       d.gateway_client_id = :cid AND d.spend_date = :date
  删掉  AND d.reserved_usd + d.settled_usd + :est <= :quota  这一行，其余完全相同。
  ```
  仍插入 reservation 行、仍累加 `reserved_usd`（记账与崩溃恢复需要它们），只是不做上限判定。
- **D 必须在发起上游调用前完成**，与 [§5.1 意图先行](./01-architecture.md) 的 attempt 落库同属"发请求前的同步写"。

**签发与轮换**（[09](./09-admin-api.md) 管理 API）：签发时生成随机明文 → 存哈希 → **明文只返回一次**；吊销即置 `status='revoked'` 并记 `revoked_at/revoke_reason`；轮换 = 新签发 + 旧的宽限期后吊销。

**服务 FR/AC**：FR-094（不展示完整凭证）、FR-113（**上游 Key 明文 ≠ 入站凭证明文**，入站强制哈希）；新增 AC 见 [14](./14-acceptance-matrix.md)。

---

## 2quater. 事务的调用者与线程模型

> 第 28 轮 [P0]：文档定义了八个事务，但**没说谁在什么线程上调用、失败了怎么办** —— 开发无法判断哪些在请求路径上（影响 P99）、哪些能异步。

| 事务 | 调用者 | 线程 | 失败处置 |
| --- | --- | --- | --- |
| C′ 落 `requests` | `protocol` | **请求 goroutine（同步）** | 返回 500，不进调度 |
| B 阶段 RPM 闸 | `protocol` | 同上 | 0 行 → 429 |
| `dispatch`（首跳） | `executor` | 同上，**发上游调用前** | 四值分支：`quota_ok=0` → 429；`capacity_ok=0` → 换候选；`exp_ok=0` → 回落非 canary / probe 跳过；异常 → 500 |
| `commit_first_actionable` | `executor` | 同上，**放行首字节前** | 中断下游流（[03 §3.0](./03-upstream-layer.md)） |
| `finalize_upstream` | `executor` | 同上，**放行终帧前** | 中断下游流，按 ③b2 结算 |
| `closeout_attempt` | `executor` | 同上，接管时 | 记 P2 告警；**不阻塞**下一跳（恢复扫描会兜底） |
| `dispatch_next` | `executor` | 同上 | 0 行 → 放弃该跳，按全候选不可用处置 |
| `finalize_delivery` | **outbox 投递器** | **后台 goroutine** | 重试；`ledger_outbox` 保留未投递行 |
| `finalize_recovery` | **恢复扫描任务** | **后台 goroutine**（周期 + 启动时） | 记日志重试；`SKIP LOCKED` 保证多实例安全 |
| `adjust` | 管理 API | **HTTP handler goroutine** | 三态判定（[§2bis](#2bis-网关调用方凭证域入站鉴权对抗性审查新增)） |

**在请求路径上的同步写共 4 次**（C′、dispatch、首字、终帧）——这是 [AC-34/36](./14-acceptance-matrix.md) 压测要盯的对象，也是允许**组提交**优化的地方。

---

## 2ter. 数据许可域（FR-093 / AC-14，**一期建表但默认放行**）

> **此前的空白**（对抗性审查第 6 轮）：FR-093 与 AC-14 只有需求文字（"按租户、数据类型、地区和业务等级定义允许使用的渠道，且在价格比较前执行"），[15 S4](./15-scope-and-preflight.md) 又声称"实现但默认放行、二期开启零改动"——但**没有任何表或 API 承载这四个维度**，"零改动开启"无从谈起，AC-14 也无法执行。本节补齐最小可实现模型。

```sql
-- 数据许可规则：四维请求属性 × 渠道 → allow/deny
CREATE TABLE data_policies (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  -- 四个匹配维度，'*' = 通配（NOT NULL + 哨兵，避免 NULL 参与唯一约束）
  tenant_id     TEXT NOT NULL DEFAULT '*',
  data_class    TEXT NOT NULL DEFAULT '*',     -- 数据级别，如 'public'/'internal'/'sensitive'
  region        TEXT NOT NULL DEFAULT '*',
  business_tier TEXT NOT NULL DEFAULT '*',
  channel_id    BIGINT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  effect        TEXT NOT NULL CHECK (effect IN ('allow','deny')),
  note          TEXT,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, data_class, region, business_tier, channel_id, effect)
);

CREATE INDEX idx_data_policies_channel ON data_policies(channel_id);
```

**开关**：`config_params` 新增 `data_policy_enabled`（bool，**默认 `false`**，`is_critical=true` 需二次确认）。

**求值算法**（`selector` [§1.1 序 3](./05-scheduling-and-operations.md)，**先于价格比较**，纯内存快照，无查库）：

```
输入：请求四元组 Q = (tenant_id, data_class, region, business_tier)、候选 channel C
0. data_policy_enabled = false        → 放行全部候选（一期默认路径，AC-14 一期表现）
1. 存在匹配行 (effect='deny',  C)     → 排除 C，排除原因 'data_policy_denied'
2. 存在匹配行 (effect='allow', C)     → 保留 C
3. 无任何匹配行                        → 排除 C，排除原因 'data_policy_no_match'（**默认拒绝，保守**）
匹配定义：四个维度上逐一满足 (行值 = '*' OR 行值 = Q 对应值)
```

- **deny 一票否决 → 求值与规则顺序无关**，无最长匹配/优先级，可预测、可单测。
- **属性来源：四个维度全部取自鉴权命中的 `gateway_clients` 行**（`tenant_id`/`region`/`business_tier`/`data_class`，NULL → `'*'`）。
  > 🔒 **铁律：不接受任何请求头/请求体承载的数据分级。** 曾考虑用 `X-Data-Class` 让调用方自报——**已否决**：调用方自报 `public` 即可绕开对敏感数据的渠道限制，等于该功能形同虚设。分级只能由**签发凭证时**由管理员写死在 `gateway_clients` 上；若同一调用方需要多个数据级别，**签发多把凭证**，而非让它自选。
  > 实现要求：`protocol` 层解析出的任何 `X-Data-Class` 类请求头**必须被丢弃且不进入求值上下文**，并在 CI 加断言（伪造该头不改变求值结果，见 AC-14）。
- **排除原因入快照**：两种排除原因写 `requests.decision_snapshot.excluded[]`，供 AC-14 断言与排障（[12](./12-debuggability.md)）。

**管理 API**（[09 §5](./09-admin-api.md)）：`GET/POST/DELETE /admin/data-policies`；`POST /admin/data-policies/simulate` **传 `gateway_client_id`**（不是裸四元组），服务端据该凭证行取属性后求值，返回"允许渠道列表 + 每个被排除渠道的原因"——与真实请求路径**走同一段求值代码**，否则模拟结果不具备验收效力（**AC-14 的可执行判定入口**）。

**服务 FR/AC**：FR-093（P1，默认关闭）、AC-14。

---

## 3. 价格版本域（不可覆盖版本 + 美元口径）

> FR-012：每次价格确认形成**不可覆盖版本**；FR-013：每请求关联决策时价格版本；FR-018：统一美元、一期 1:1。价格版本由自研核心持有，`attempts.price_version_id` 回指本表实现历史成本复算。

```sql
-- 价格版本（FR-010/012/013/017/018）：append-only，永不 UPDATE 已生效行
-- ⚠️ 基础价与倍率**必须分表**（对抗性审查发现）：
--    基础价的作用域是 (channel, model)，而分组/Key 倍率的作用域是 binding。
--    混在一张表里 → 同渠道多 Key/多分组时行归属不明，selector 选不出确定的当前价，
--    历史复算可能用到不相干的倍率，直接破坏 FR-013「历史成本可复算」。

-- ① 基础价版本：作用域 (channel, model)
CREATE TABLE price_versions (
  id              UUID PRIMARY KEY,           -- UUIDv7
  channel_id      BIGINT NOT NULL REFERENCES channels(id),
  model_id        BIGINT NOT NULL REFERENCES models(id),
  input_price     nonneg_usd NOT NULL,        -- 每计费单位（归一为美元）
  output_price    nonneg_usd NOT NULL,
  cache_price     nonneg_usd,                 -- 缓存价（走折扣）
  billing_unit    TEXT NOT NULL DEFAULT 'per_1m_token',
  currency        TEXT NOT NULL DEFAULT 'USD',-- 一期仅名称，不换汇（FR-018/AC-17）
  data_source     TEXT NOT NULL,              -- auto_collect / manual
  queried_at      TIMESTAMPTZ NOT NULL,       -- 查询时间（FR-012）
  effective_at    TIMESTAMPTZ NOT NULL,       -- 生效时间（FR-012）
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ② 倍率版本：作用域 binding（承载分组倍率与 Key 倍率）
CREATE TABLE multiplier_versions (
  id              UUID PRIMARY KEY,           -- UUIDv7
  binding_id      BIGINT NOT NULL REFERENCES bindings(id),
  group_multiplier NUMERIC(12,6),             -- 用户组倍率（FR-010）
  key_multiplier  NUMERIC(12,6),              -- Key 倍率（FR-003）
  data_source     TEXT NOT NULL,
  queried_at      TIMESTAMPTZ NOT NULL,
  effective_at    TIMESTAMPTZ NOT NULL,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 价格变化记录（FR-014/017）：涨价限流 / 降价逐步；供按模型/渠道/Key 查询影响范围
CREATE TABLE price_change_log (
  id              UUID PRIMARY KEY,
  channel_id      BIGINT NOT NULL REFERENCES channels(id),
  model_id        BIGINT NOT NULL REFERENCES models(id),
  from_version_id UUID REFERENCES price_versions(id),
  to_version_id   UUID NOT NULL REFERENCES price_versions(id),
  direction       TEXT NOT NULL CHECK (direction IN ('increase','decrease','stale','confirmed')), -- FR-014/015
  confirmed       BOOLEAN NOT NULL DEFAULT false, -- 降价需再次确认或实际扣费验证（FR-014/AC-03）
  detected_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

- **不可覆盖**：两表均仅 INSERT。
  「当前生效价」= 该 `(channel,model)` 下 **`confirmed = true`** 且 `effective_at <= now()` 的最新 `price_versions` 行；
  「当前生效倍率」= 该 `binding_id` 下最新 `multiplier_versions` 行。
  > ⚠️ **`confirmed` 这个条件不能漏**（第 42 轮：本行与下方成本公式此前都只写了 `effective_at` 最新）：
  > 采集到的**降价**版本插入时 `confirmed=false`（[04 §价格变更留痕](./04-collector-adapter.md)）。
  > 漏掉该条件，一个可能是误读的低价会**立刻**进入低价优选，AC-03 的"确认后才逐步增加"完全绕过。
  > 索引 `idx_price_cur` 就是按 `WHERE confirmed` 建的部分索引——漏条件还会走不上索引。
- **成本计算**（第 18 轮 [high] 修正；第 40 轮把散文词绑到列）：

  > ⚠️ 此前写的是"基础价 × 倍率"这类**散文词**，`input_price` / `output_price` / `cache_price` /
  > `group_multiplier` / `key_multiplier` 五列建好后**全库零引用**——开发无从知道哪个词对应哪一列，
  > 尤其 `cache_price` 完全没进过公式，而 `attempt_usage.prompt_cached_tokens` 是采集得到的
  > → 缓存命中部分会被按输入价计费（第 40 轮悬空列检查发现）。

  ```
  设 pv = 当前生效 price_versions 行（该 channel+model，**confirmed=true** 且 effective_at<=now() 的最新行）
     mv = 当前生效 multiplier_versions 行（该 binding 的最新行）
     u  = unit_tokens(pv.billing_unit)：per_1m_token→1_000_000 / per_1k_token→1_000 / per_token→1
     m  = COALESCE(mv.group_multiplier, 1) × COALESCE(mv.key_multiplier, 1)

  cached   = COALESCE(au.prompt_cached_tokens, 0)              -- 缓存命中的输入 token
  fresh_in = GREATEST(COALESCE(au.prompt_tokens,0) - cached, 0) -- 未命中的输入 token
  p_cache  = COALESCE(pv.cache_price, pv.input_price)           -- 无缓存价的渠道退回输入价

  成本 = ( fresh_in                        × pv.input_price
         + cached                          × p_cache
         + COALESCE(au.completion_tokens,0)× pv.output_price ) / u × m
  ```

  - **`prompt_tokens` 含缓存部分**：上游回传的 `promptTokens` 是**总输入**，缓存命中数另给一列。
    直接拿 `prompt_tokens × input_price` 会把缓存部分按全价算——长会话下这是主要成本来源，
    误差不是小数点问题。故必须先扣减再分段计价。
  - **`cache_price IS NULL` 退回 `input_price`**（而非 0）：不知道折扣就按不打折算，方向保守。
  - **两个倍率相乘**：`group_multiplier` 是用户组倍率（FR-010）、`key_multiplier` 是 Key 倍率（FR-003），
    二者独立叠加；任一为 NULL 视作 1（未登记 ≠ 免费）。
  - **`upstream_keys.key_multiplier` 是登记态，`multiplier_versions.key_multiplier` 是版本快照**——
    算成本一律用后者（前者会被采集器覆盖，历史复算会失真）。
  > ⚠️ 原文写作 `成本 = 基础价 × 倍率 × token`，**漏掉了按 `billing_unit` 的缩放**。而 `billing_unit` 默认就是 `per_1m_token`——照原式实现会把每一笔成本**放大 1,000,000 倍**，预留、配额、错误预算、告警阈值全部失真。
  > **实现要求**：入库时**不做**归一（保留上游原始口径便于对账与排障），缩放只发生在算成本的这一处，且 `unit_tokens` 必须由 `billing_unit` 查表得出，**不得硬编码**。
  > **CI 断言**：给定 `per_1m_token` 单价 3.0、1000 token → 成本必须是 `0.003` 而非 `3000`。
- **两个版本 id 都必须落到 attempt**，历史复算时同时取回才能还原当时的完整计价输入（FR-013/AC-02）。
- 价格过期/查询失败保守处理（FR-015）由决策层用 `queried_at` 判新鲜度（§11 默认 6h/24h/48h），不在本表建标志位。

**索引**

```sql
-- ⚠️ 降价确认（FR-014/AC-03，第 29 轮 [P0]）：AC-03 要构造「降价版本 confirmed=false」，
--    但本表原先**没有这一列** —— 开发不知道确认状态放哪、selector 该读哪张表。
ALTER TABLE price_versions ADD COLUMN confirmed BOOLEAN NOT NULL DEFAULT true;
ALTER TABLE price_versions ADD COLUMN confirmed_by TEXT;
ALTER TABLE price_versions ADD COLUMN confirmed_at TIMESTAMPTZ;
-- 采集到**降价**时插入 confirmed=false 的新版本；涨价与首次采集直接 confirmed=true。
-- selector 的「当前价格」= 该 (channel, model) 下 confirmed=true 且 effective_at 最大的一行。
CREATE INDEX idx_price_cur ON price_versions(channel_id, model_id, effective_at DESC)
  WHERE confirmed;
CREATE INDEX idx_mult_cur  ON multiplier_versions(binding_id, effective_at DESC);
```

**服务 FR/AC**：FR-010、FR-012~018；AC-02（历史按原版本复算）、AC-03（降价确认后逐步）、AC-17（美元口径 1:1）。

---

## 4. 请求与逐 Attempt 账本域（本篇核心）

> 一个外部请求下挂多条 attempt（首字前接管 → 多跳）。每 attempt 记 binding、status、errorMessage、上游 externalID、逐尝试 metrics、是否被 SLA 取消、继续计费标记、隐藏重试补算。取消按 errorMessage 归并（AC-30）。**账本为单一真相源，无对账环节**（[11](./11-decision-full-selfbuilt.md)）。**不存正文/头/体**（FR-112）。

### 4.1 外部请求

```sql
-- 外部请求（FR-097/098、FR-092）：一次调用方请求；按月分区（保留≥180天，FR-112）
CREATE TABLE requests (
  id            UUID NOT NULL,                -- UUIDv7（时间有序，多实例无冲突，FR-110）
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  gateway_client_id BIGINT NOT NULL REFERENCES gateway_clients(id), -- 入站调用方（FR-120）；配额归集与审计必需
  alias_id      BIGINT REFERENCES model_aliases(id), -- 调用方选的别名（承载策略，FR-062/117）
  policy_id     BIGINT REFERENCES routing_policies(id),
  sla_level     TEXT,                         -- 冗余快照，便于按等级统计（FR-090/091）
  tenant_id     TEXT,                         -- 个人/内部默认单租户可空（FR-093 默认关闭）
  session_id    TEXT,                         -- 连续会话键（FR-050/051）；无正文，仅标识
  request_type  TEXT NOT NULL,               -- 'chat_completions' / 'responses'（FR-111/AC-26）
  is_streaming  BOOLEAN NOT NULL DEFAULT false,
  probe_kind    TEXT NOT NULL DEFAULT 'normal' CHECK (probe_kind IN ('normal','probe')), -- 业务测活标记（FR-092）
  -- 决策快照（FR-097）：候选/排除原因/选择——元数据 JSON，不含正文
  decision_snapshot JSONB,                    -- {candidates:[...], excluded:[{binding,reason}], chosen, cache_pred, balance, capacity}
  -- 请求级结果（FR-098）：最终对外结果
  -- 请求推进到哪一阶段（第 18 轮 [critical]）：C′ 提前插入 requests 后，
  -- 崩溃可能发生在 selector **之前** —— 此时零 attempt、无预留、无 decision_snapshot，
  -- 与「全候选不可用」「预留失败 429」外观完全相同，恢复扫描无从区分该判 unavailable 还是 failed。
  stage         TEXT NOT NULL DEFAULT 'authenticated'
                  CHECK (stage IN ('authenticated',      -- C′ 已落库，尚未进 selector
                                   'planned',            -- selector 已产出非空 RoutePlan
                                   'no_candidates',      -- selector 输出空候选集
                                   'reservation_rejected',-- D 阶段原子预留返回 0 行（429）
                                   'dispatched')),       -- 已发起上游调用（至少一条 attempt）
  final_status  TEXT NOT NULL DEFAULT 'pending'
                  CHECK (final_status IN ('pending','completed','failed','canceled','unavailable','interrupted')),
                  -- ⚠️ 'pending' 是**唯一非终态**；恢复扫描必须把它推到某个终态并同步终结
                  --    对应的 client_reservations 行（§2bis 统一终结事务），否则配额永久泄漏
  effective_ttft_ms INTEGER,                  -- 对外有效首字（取被提交 attempt 的自算 TTFT，AC-31）
  had_takeover  BOOLEAN NOT NULL DEFAULT false, -- 是否发生首字前接管（FR-076/092）
  PRIMARY KEY (id, created_at)                -- 分区键须入主键
) PARTITION BY RANGE (created_at);
```

### 4.2 逐 Attempt 账本（每外部请求 ≥1 条）

```sql
-- 逐尝试账本（FR-097/098、FR-070、FR-080、FR-119；AC-09/12/16/30/32）
CREATE TABLE attempts (
  id                UUID NOT NULL,            -- UUIDv7
  request_id        UUID NOT NULL,            -- 关联同一外部请求（归集主键）
  request_created_at TIMESTAMPTZ NOT NULL,    -- 冗余分区键，与 requests 对齐
  attempt_no        SMALLINT NOT NULL,        -- 该请求内第几跳（1=主，2=接管…）
  binding_id        BIGINT NOT NULL REFERENCES bindings(id), -- 渠道+key+url（路由资源）
  -- 该跳的费用上界（第 37 轮补）：恢复结算要按「已终结但无用量 → 该跳单跳保守估算」汇总，
  -- 而 RoutePlanEntry.SingleHopUSD 只在**内存**里——崩溃后恢复任务读不到它。必须落库。
  single_hop_est_usd nonneg_usd,
  price_version_id  UUID REFERENCES price_versions(id),      -- 决策时的**基础价**版本（FR-013/AC-02）
  multiplier_version_id UUID REFERENCES multiplier_versions(id), -- 决策时的**倍率**版本（binding 级）
  -- 两者合起来才是该 attempt 的完整计价输入，缺一不可复算
  role              TEXT NOT NULL CHECK (role IN ('primary','takeover','retry','canary','probe')), -- FR-076/079
                    -- canary：一期受控验证——**本来就要发的真实业务请求**被分给待验证 binding（不额外产生费用）
                    -- probe：主动测活（**一期第二轨**）——为探测而额外发起的请求（[05 §2.1~2.3](./05-scheduling-and-operations.md)）

  -- ── 状态：上游原始 vs 我方归并（AC-30 核心）──
  -- ⚠️ 第 44 轮删除了 `gateway_status`（原 beta5 `execution.status` 存证列）：
  --    C6 只改了它的注释、没删列，而**全库没有任何规则读写它**（收窄后的悬空列检查抓出）。
  --    转向自研后（[11](./11-decision-full-selfbuilt.md)）数据面没有外部网关，上游是中转站、
  --    只回 HTTP 状态与错误体，不存在"网关自身状态枚举"可存；且原注释自己写着
  --    "永不作为判定依据""多为 NULL"。归并所需的输入只有 error_message + 我方观测事实。
  error_message     TEXT,                     -- 上游错误原文（元数据，非正文）；**归并的唯一外部输入**
  -- 归并后的取消/失败口径：外部实现常把 canceled 只用于客户端取消，上游断开/内部超时归 failed
  -- → 我方按 error_message + 观测事实自行归并（AC-30、ISSUE-001 假设2 语义澄清）
  cancel_reason     TEXT CHECK (cancel_reason IN
                      ('none','client_disconnect','sla_takeover','upstream_disconnect','internal_timeout','upstream_error')),
  attempt_status    TEXT NOT NULL DEFAULT 'pending'
                      CHECK (attempt_status IN (
                        -- 非终态（两者都须被 §4.2bis 租约扫描覆盖）
                        'pending',            -- 已落意图，未提交
                        'committed',          -- 已提交输出给下游，流未结束
                        -- 终态
                        'completed','failed','canceled_by_sla','canceled_by_client',
                        'unknown_billing','interrupted')),
                      -- ⚠️ canceled_by_client：**客户端主动断开**（Codex 用户按 Ctrl-C 是高频操作）。
                      --    此前枚举里没有它，三个可选项全错：记 canceled_by_sla 是语义错误
                      --    （不是我们取消的）、记 failed 会污染渠道成功率（渠道没问题）、
                      --    记 completed 更错。故单列一态：
                      --      · **不计入**渠道成功率（不是渠道的锅，与 unknown_billing/interrupted 同档）
                      --      · **不计入**用户 SLA 失败（PRD 术语：用户主动取消不计入）
                      --      · 成本**计入**（上游已经生成的 token 照样收费）
                      -- ⚠️ unknown_billing：上游**已发出**但进程在写入首条 outbox 事件前崩溃 →
                      --    可能已计费、结果完全不明。
                      -- ⚠️ interrupted：已提交输出（见过首字）但**未见终帧**即崩溃 →
                      --    **确定已计费**、用量未知。与 unknown_billing 的区别是计费确定性，
                      --    两者都按保守估算计入成本；**渠道健康**统计不计入（崩溃不是渠道的锅），
                      --    但**用户侧 SLA 必须计入失败**（FR-071/AC-12）。两套口径见 §4.2bis 不变式 3。
                      -- ⚠️ `pending` 与 `committed` 都是**非终态**：只覆盖 pending 的扫描
                      --    会让首字后崩溃的 attempt 永久悬挂（对抗性审查第 7 轮 critical）。见 §4.2bis

  -- ── 三个**互相独立**的持久化事实（对抗性审查第 8 轮 [high]）──
  -- ⚠️ 不可用 attempt_status='committed' 反推"见过首字"：[03 §3.2](./03-upstream-layer.md) 的
  --    ShouldCommit 在**空终态/错误终态**上也为 true 而 HasTTFTOutput 为 false。
  --    若恢复扫描按"committed ⇒ 见过首字"判定，会把"空响应已提交、关单前崩溃"
  --    误写成 interrupted + 人工核对，而它的真实终态是 completed/failed。
  response_committed_at TIMESTAMPTZ,          -- ShouldCommit 首次为真、**且已落库**的时刻
  commit_trigger    TEXT CHECK (commit_trigger IN ('actionable','buffer_limit')),
                                              -- 'actionable' = 真见到可执行输出；
                                              -- 'buffer_limit' = T2 缓冲上限强制提交（非真首字，
                                              --   has_ttft_output=false、ttft 为 NULL、不计入 TTFT 统计）
  -- ── 上游事件到达时刻（**同步写之前**）：与下游交付时刻配对，度量我们自己的开销 ──
  upstream_first_actionable_at TIMESTAMPTZ,   -- 上游首个 ShouldCommit 事件**到达**时刻（早于 response_committed_at）
  upstream_terminal_at         TIMESTAMPTZ,   -- 上游终帧**到达**时刻（早于 finalize_upstream 提交）

  -- ── 本进程下游写入完成（[03 §3.0](./03-upstream-layer.md)）：与上游事实**独立**，不可互相推导 ──
  -- ⚠️ 只证明「我们写出去了」，**不证明客户端收到了**（对端是 Caddy）。端到端确认需客户端回执，
  --    而主力客户端 Codex CLI 不可要求配合 → 该残余不确定性已知并接受，不写进任何保证。
  -- DB 提交与 socket 写出无法原子化，反方向窗口不可消除 → 必须分开记，冲突时按"未确认=未交付"取保守解释。
  -- 不参与计费（成本只看上游事实），故可异步经 outbox 落库。
  downstream_first_byte_written_at TIMESTAMPTZ,       -- 首字节已写出下游 socket
  downstream_write_completed_at  TIMESTAMPTZ,       -- 终帧已写出下游 socket / 流正常关闭
  has_ttft_output   BOOLEAN NOT NULL DEFAULT false, -- HasTTFTOutput 是否曾为真（决定 TTFT 是否有效）
  terminal_event    TEXT CHECK (terminal_event IN
                      ('completed','empty_completed','error','incomplete')),
                                              -- 观察到的上游终帧类型；NULL = **未见终帧**
                                              -- 恢复扫描据此区分"流真的断了"与"已收完只是没关单"

  -- ── 逐尝试 metrics（自算，AC-31）──
  content_aware_ttft_ms INTEGER,              -- 自算内容感知首字（排除 role-only/空SSE/心跳，AC-31/假设3/6）
                                              -- has_ttft_output=false 时恒为 NULL
  gateway_reported_ttft_ms INTEGER,           -- 上游/中间层若回传首字类字段 → **仅存证、永不采信**（AC-31）
                                              -- 命名保留"gateway_"前缀只为兼容既有引用；语义是"非我方自算的那个值"
  full_latency_ms   INTEGER,                  -- 总延迟（请求进入→完整结束）
  upstream_latency_ms INTEGER,                -- 上游耗时（发往上游→完整结束）
  -- 决策/网关自身开销 = full_latency_ms − upstream_latency_ms，用于 FR-110「P99≤50ms」自监控（测法见 06 §6）
  -- ⚠️ 该差值**测不到同步写**（首字同步写落在 upstream_latency_ms 内被抵消，第 11 轮 [high]）。
  --    同步写代价用下面两个**派生指标**度量（无需新增列，由上面四个时刻算出，[14 判定口径](./14-acceptance-matrix.md)）：
  --      downstream_ttft_delay_ms   = downstream_first_byte_written_at − upstream_first_actionable_at
  --      downstream_finish_delay_ms = downstream_write_completed_at  − upstream_terminal_at
  output_tokens_per_s NUMERIC(12,3),          -- 输出速度（FR-040）；**分母 = full_latency_ms − content_aware_ttft_ms**
                                              -- 只算生成阶段；用总延迟会把慢首字渠道误判为「生成慢」
  stream_broken     BOOLEAN NOT NULL DEFAULT false, -- 已输出首字后中断=完整失败，不拼接（FR-078/AC-12）

  -- ── SLA 取消 / 继续计费 / 重复费用（FR-080、AC-32）──
  canceled_by_sla   BOOLEAN NOT NULL DEFAULT false, -- 是否被 SLA 主动取消（首字前接管/期限到达）
  cancel_propagated BOOLEAN,                  -- 取消是否已传播到上游止损（AC-32；自研层 Close() 拆上游连接，[03 §6](./03-upstream-layer.md)）
  continue_billing  BOOLEAN NOT NULL DEFAULT false, -- 取消后是否仍继续计费（FR-080 记录继续计费情况）
  is_duplicate_cost BOOLEAN NOT NULL DEFAULT false, -- 未取消重复请求费用（FR-058/8.4）

  -- ── 隐藏重试补算（FR-119、假设6）──
  -- 外部调用恒为 1 次（=本 attempt）；上游实际调用可能 >1（ccLoad Codex 400 body-rewrite → 2 次 POST，仅留一条审计）
  upstream_call_count SMALLINT NOT NULL DEFAULT 1, -- 实际上游调用数（补算后）
  hidden_retry_detected BOOLEAN NOT NULL DEFAULT false,
  hidden_retry_kind  TEXT,                    -- 如 'codex_400_strip_thinking'（假设6补验）
  -- ⚠️ 上面三列（upstream_call_count / hidden_retry_detected / hidden_retry_kind）**一期恒为默认值**：
  --    §11 开放点 5 已裁定一期不做隐藏重试推断（我们自己不发，上游中转站内部若有也不可观测）。
  --    保留建表只为二期接入会隐藏重试的通道时零改表。**不是漏实现**（第 40 轮明示）。

  -- ── 上游关联键（自研直连，旁路观察提取）──
  upstream_response_id    TEXT,               -- 上游响应 id（如 resp_.../chatcmpl-...），用于排障关联

  started_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  ended_at          TIMESTAMPTZ,

  -- ── attempt 租约（§4.2bis 悬挂检测）──
  external_call_started_at TIMESTAMPTZ,       -- 上游请求实际发出的时刻；NULL=尚未发出（崩溃则未计费）
  lease_heartbeat_at TIMESTAMPTZ,             -- 持有实例的心跳；**插入 attempt 时即写入非 NULL**（防漏扫）
  lease_owner       TEXT,                     -- 持有该 attempt 的实例标识

  PRIMARY KEY (id, request_created_at),
  -- ⚠️ 恢复扫描用 max(attempt_no) 判定「最后一跳」；无此约束时重复 attempt_no 会让最后一跳不唯一，
  --    同一 request 可能被处理多次（第 18 轮 [high]；第 19 轮修正：此约束曾被误加到 session_prefix_ledger）
  UNIQUE (request_id, request_created_at, attempt_no)
) PARTITION BY RANGE (request_created_at);
```

### 4.2bis 悬挂 Attempt 检测（**补齐 outbox 的盲区**）

> **outbox 重放挡不住这个窗口**：进程若在"**已向上游发出请求**、但**尚未写入首条 outbox 事件**"之间崩溃，则 `attempts` 停在 `pending` 且**无任何事件可重放**。自研账本没有外部对账源，这笔**可能已计费**的调用会永久悬着，污染成本与 SLA 统计。（对抗性审查发现；这是 [01 §5.1](./01-architecture.md) 持久化协议的盲区。）

**租约机制**：

| 字段 | 含义 |
| --- | --- |
| `external_call_started_at` | 上游请求**实际发出**的时刻。**NULL 即代表尚未发出** —— 此时崩溃可安全判定为**未计费**，直接置 `failed` |
| `lease_heartbeat_at` | 持有实例每 N 秒更新一次；**停更即视为实例已死** |
| `lease_owner` | 持有该 attempt 的实例标识 |

**写入顺序（冻结，不可交换）**：

```
① INSERT attempt(status='pending', lease_owner, lease_heartbeat_at = now())   ← 心跳与行同时写，绝不留 NULL
② UPDATE attempt SET external_call_started_at = now()                          ← 紧邻发请求之前
③ 发出上游请求
```

> ②③ 之间的窗口内崩溃 → `external_call_started_at` 已置但请求可能未真正发出：**按"已发出"保守处理**（宁可标 `unknown_billing` 人工核对，不可漏判为未计费）。
> 提交首字时（③ 之后）另有 `UPDATE attempt SET attempt_status='committed'`——它使 attempt 离开 `pending`，故恢复扫描必须同时覆盖 `committed`（见下）。
> `lease_heartbeat_at` 在 ① 即写入非 NULL，**杜绝"已发出但无心跳"的漏扫**。

**恢复扫描**（启动时 + 周期性，与 outbox 重放并列）——**必须覆盖两类，缺一即产生永久 pending**：

```sql
-- ⚠️ 必须是 **request 级**扫描，不能扫 attempt 状态（第 14 轮 critical，同一 bug 第三次）：
--    两阶段关单后 attempt 先进终态、request 后进终态 → K4（attempt 已 completed/failed、
--    request 仍 pending）用 attempt_status IN ('pending','committed') **永远扫不到**，
--    第 13 轮宣称的「③c/K4 真的可达」与实现 SQL 直接冲突。
-- ⚠️ 唯一非终态是 requests.final_status='pending'；它是恢复扫描的**唯一入口**。
-- ⚠️ 租约仍挂在 attempt 上（executor 每 ≤20s 续写），故用最后一跳的租约判活。
--    NULL 必须显式覆盖：SQL 的 < 对 NULL 恒不成立（第 5 轮 critical）。
SELECT r.id, r.created_at, a.*
  FROM requests r
  -- ⚠️ 必须 LEFT JOIN（本轮自查 [critical]）：INNER JOIN 会漏掉**零 attempt 的请求**——
  --    全候选不可用、预留失败 429 都属此类，它们有 requests 行却没有任何 attempt，
  --    用 INNER JOIN 永远捞不到 → 永久停留 pending。
  LEFT JOIN attempts a
    ON a.request_id = r.id AND a.request_created_at = r.created_at
   AND a.attempt_no = (SELECT max(attempt_no) FROM attempts x
                        WHERE x.request_id = r.id AND x.request_created_at = r.created_at)
 WHERE r.final_status = 'pending'
   AND r.created_at > now() - INTERVAL '7 days'          -- 限定分区范围，避免全表扫
   AND (
     -- 有 attempt：按最后一跳的租约判活
     (a.id IS NOT NULL AND (a.lease_heartbeat_at IS NULL
                            OR a.lease_heartbeat_at < now() - INTERVAL '60 seconds'))
     -- 零 attempt：没有租约可判，按请求自身年龄判活
     OR (a.id IS NULL AND r.created_at < now() - INTERVAL '60 seconds')
   )
   -- ⚠️ 反连接必须写进 SQL 本体，不能只写在说明里（第 15 轮 [high]）：
   --    downstream_write_completed 已写入 outbox 但尚未投递时，
   --    attempts.downstream_write_completed_at 仍是 NULL → 会被误判成 ③c/interrupted，
   --    与 AC-35「重放后 completed」直接冲突。
   AND NOT EXISTS (
         SELECT 1 FROM ledger_outbox o
          WHERE o.attempt_id = a.id
            AND o.request_created_at = a.request_created_at
            AND o.event_type IN ('downstream_first_byte','downstream_write_completed')
            AND o.delivered_at IS NULL)
   FOR UPDATE OF r SKIP LOCKED;
```

**启动与执行顺序被冻结**（第 14 轮 [high]）：

```
实例启动 / 周期任务：
  ① 先 drain ledger_outbox（含 downstream_first_byte / downstream_write_completed）
  ② 再跑上面的恢复扫描
```

> ⚠️ 顺序反了会误判：socket write 已返回、`downstream_write_completed` 的 outbox 行已写但**尚未投递**时，`attempts.downstream_write_completed_at` 仍是 NULL；若恢复扫描先跑，会把一个**已经完整写完**的请求判成 ③c/`interrupted`。
> 该跳过条件**已写进上面的 SQL 本体**（`NOT EXISTS` 反连接），不是仅存在于说明里——照 SQL 实现即正确。

捞出后调用 **`finalize_recovery`**（骨架同 `finalize_upstream`，但**执行 ⑤ 写 `requests.final_status`**），**按"上游发出了吗、见到首字了吗"两个事实分流**，每种情形都推到**终态**并在**同一个 `finalize` 事务**（[§2bis](#2bis-网关调用方凭证域入站鉴权对抗性审查新增)）内终结配额预留：

| # | 对应 [03 §3.0](./03-upstream-layer.md) | 判据（读 attempt 的三个事实列） | attempt 终态 | request 终态 | reservation | 告警 |
| --- | --- | --- | --- | --- | --- | --- |
| ⓪a | — | 无 attempt 且 `stage='no_candidates'` | — | `unavailable` | 无预留 → 跳过闸门 | **P1**（AC-15） |
| ⓪b | — | 无 attempt 且 `stage='reservation_rejected'` | — | `failed` | 无预留 → 跳过闸门 | 无（429 已回给调用方） |
| ⓪c | — | 无 attempt 且 `stage='authenticated'`（**C′ 之后、selector 之前崩溃**） | — | `failed` | 无预留 → 跳过闸门 | P3（内部中断，非用户可见错误） |
| ⓪d | — | 无 attempt 且 `stage='planned'`（**selector 之后、预留之前崩溃**） | — | `failed` | 无预留 → 跳过闸门 | P3 |
| ① | — | **该 request 的全部 attempt 都未发出**（不存在 `external_call_started_at IS NOT NULL` 的行） | `failed` | `failed` | `abandoned`（**释放预留**，`settled_usd` 不加） | 无（确定未计费） |
| ①bis | — | 最后一跳未发出，但**此前有 attempt 已发出**（多跳接管场景） | 末跳 `failed`；前序跳按 ⓐ | `failed` | **`settled`**，`actual_usd` = Σ 已有 attempt（**不可 `abandoned`**——hop1 的钱已经花了） | P3 |
| ② | **K1** | `external_call_started_at NOT NULL` 且 `response_committed_at IS NULL` | `unknown_billing` | `failed` | `settled`，全跳汇总，`needs_manual_review=true` | **P2** |
| ③b1 | **K2** | `response_committed_at NOT NULL`、`terminal_event IS NULL`、`downstream_first_byte_written_at IS NULL` | `interrupted` | **`failed`** | `settled`，全跳汇总，待核对 | **P3** |
| ③b2 | **K3** | `response_committed_at NOT NULL`、`terminal_event IS NULL`、`downstream_first_byte_written_at NOT NULL` | `interrupted` | **`interrupted`** | `settled`，全跳汇总，待核对 | **P3** |
| ③c | **K4** | `terminal_event NOT NULL`、`downstream_write_completed_at IS NULL` | **由 `terminal_event` 决定**：`completed`/`empty_completed` → `completed`；`error`/`incomplete` → **`failed`** | **`interrupted`** | `settled`，用**实际值**，`needs_manual_review=false` | 无 |

> **没有"全部写完"这一恢复分支**（第 14 轮 [high] 修正）：`finalize_delivery` 把「写 `downstream_write_completed_at`」与「推 `final_status`」放在**同一事务**，因此**不存在**「列已非空但 request 仍 `pending`」的稳定状态——曾经的 ③a／③d 是**不可达分支**，已删除。已写完的请求由**启动时先 drain outbox** 收口，根本不进恢复扫描。
>
> ⚠️ **① 的判据必须是 request 级，不能只看最后一跳**（第 25 轮 [high]）：多跳接管下 hop1 已发出（可能已计费）、hop2 刚插入未发出时崩溃，末跳确实 `external_call_started_at IS NULL`，但整笔预留**不能 `abandoned`** —— 那会把 hop1 已花的钱直接抹掉。故 ① 要求"**全部** attempt 都未发出"，否则走 ①bis 按 `settled` + Σ 结算。
>
> ⚠️ 三个事实列、三次判定，**任何一个都不能由 `attempt_status` 反推**（两阶段关单后 attempt 可能已是终态）：
> ① `response_committed_at` 分 ② 与 ③；② `terminal_event` 分 ③b 与 ③c；③ `downstream_first_byte_written_at` 分 ③b1 与 ③b2。
> 第 12 轮曾只读 `terminal_event`，把「首字写出未确认」（K2）也判成 `interrupted`，等于声称用户看到过截断流。

**`finalize_recovery` 必须用 `requests` 作闸门，不能复用 reservation 闸门**（第 22 轮 [critical]，K4 不可达第四次出现）：

> ⚠️ 第 21 轮把 `finalize` 骨架整条链改成 `FROM settled` 驱动（闸门 = reservation 的 `reserved → settled` 跃迁），修好了"迟到的 abort 篡改终态"。**但恢复路径不能复用这个闸门**：K4 的定义就是 `finalize_upstream` **已经结算过**（reservation 已是 `settled`），只差 `finalize_delivery` 没跑完。此时 `settled` CTE 返回 0 行 → 整条链一句不执行 → ⑤ 永远写不了 `requests.final_status` → **K4 永久 `pending`**。
>
> **根因**：reservation 状态是"钱结没结"的幂等键，而恢复要解决的是"**单子关没关**"。两者不是同一件事，不能共用闸门。

```sql
-- finalize_recovery：闸门是 requests，不是 reservation
BEGIN;
WITH req AS (
  SELECT r.id, r.created_at FROM requests r
   WHERE r.id = :rid AND r.created_at = :rcat AND r.final_status = 'pending'
     -- ⚠️ 第 23 轮 [high]：扫描与关单之间存在 TOCTOU —— `downstream_write_completed`
     --    的 outbox 行可能在"扫描捞出之后、关单之前"完成投递，此时该请求其实已经写完了，
     --    recovery 却仍会把它关成 `interrupted`。故**扫描判据必须在本事务内复核一遍**。
     AND NOT EXISTS (
       SELECT 1 FROM ledger_outbox o
        WHERE o.attempt_id = :last_attempt_id
          AND o.request_created_at = r.created_at
          AND o.event_type IN ('downstream_first_byte','downstream_write_completed')
          AND o.delivered_at IS NULL)
     AND NOT EXISTS (                                 -- 租约复核：期间被续租则放弃本次恢复
       SELECT 1 FROM attempts a
        WHERE a.id = :last_attempt_id
          AND a.lease_heartbeat_at > now() - INTERVAL '60 seconds')
   FOR UPDATE                                        -- 闸门：单子还没关 + 判据仍成立
),
-- ⓐ 非最后一跳的非终态 attempt → 必然是被接管取消的
prev AS (
  UPDATE attempts a SET attempt_status = 'canceled_by_sla',
         cancel_reason = COALESCE(a.cancel_reason,'sla_takeover'),
         canceled_by_sla = true, ended_at = now()
    FROM req
   WHERE a.request_id = req.id AND a.request_created_at = req.created_at
     AND a.attempt_status IN ('pending','committed') AND a.attempt_no < :last_attempt_no
  RETURNING a.id
),
-- ⓑ 最后一跳 → 按 §4.2bis 分流表定终态
last AS (
  UPDATE attempts a SET attempt_status = :attempt_terminal, ended_at = now()
    FROM req
   WHERE a.id = :last_attempt_id AND a.request_id = req.id
     AND a.attempt_status IN ('pending','committed')
  RETURNING a.id
),
-- ⓒ reservation **条件结算**（不是闸门）：仍 reserved 才结算；已 settled（K4）则跳过
resv AS (
  UPDATE client_reservations r
     -- ⚠️ 终态**参数化**（第 25 轮 [high]）：原先硬编码 'settled'，与恢复表 ① 要求的
     --    'abandoned' 直接矛盾 —— 按表实现漏计 hop1、按 SQL 实现不满足 AC-35 的 abandoned 断言。
     SET state=:resv_terminal,                    -- 'settled' | 'abandoned'
         actual_usd=:actual_usd, settle_event_key=:event_key,
         needs_manual_review=:needs_review, settled_at=now()
    FROM req
   WHERE r.request_id = req.id AND r.state = 'reserved'
  RETURNING r.gateway_client_id, r.spend_date, r.estimated_usd, r.actual_usd
),
agg AS (
  UPDATE client_daily_spend d
     SET reserved_usd = d.reserved_usd - s.estimated_usd,     -- 两种终态都扣回预留
         settled_usd  = d.settled_usd
                      + CASE WHEN :resv_terminal = 'settled'  -- 仅 settled 才记入已花费
                             THEN COALESCE(s.actual_usd, 0) ELSE 0 END
    FROM resv s
   WHERE d.gateway_client_id = s.gateway_client_id AND d.spend_date = s.spend_date
  RETURNING 1
),
-- ⓓ 释放该 request 名下**全部**残留 claim（按 binding 计数扣减）
-- ④ 释放本跳的**三套**占用（第 31 轮统一：此前只放 canary，capacity/probe 永不释放必然泄漏）——恢复时按 request 批量
rel_cap AS (
  UPDATE capacity_claims c SET state='released', released_at=now()
    FROM req WHERE c.request_id = req.id AND c.state='active' RETURNING c.binding_id),
rel_can AS (
  UPDATE canary_claims  c SET state='released', released_at=now()
    FROM req WHERE c.request_id = req.id AND c.state='active' RETURNING c.binding_id),
rel_prb AS (
  UPDATE probe_claims   c SET state='released', released_at=now()
    FROM req WHERE c.request_id = req.id AND c.state='active' RETURNING c.binding_id),
cap_agg AS (SELECT binding_id, count(*) AS n FROM rel_cap GROUP BY binding_id),
can_agg AS (SELECT binding_id, count(*) AS n FROM rel_can GROUP BY binding_id),
dec AS (
  UPDATE resource_health h
     SET concurrency_inflight = GREATEST(h.concurrency_inflight - COALESCE(c.n,0), 0),
         canary_inflight      = GREATEST(h.canary_inflight      - COALESCE(k.n,0), 0)
    FROM (SELECT binding_id FROM cap_agg UNION SELECT binding_id FROM can_agg) b
    LEFT JOIN cap_agg c ON c.binding_id = b.binding_id
    LEFT JOIN can_agg k ON k.binding_id = b.binding_id
   WHERE h.binding_id = b.binding_id
  RETURNING 1),
-- ⓔ 关单（本事务的目的）
fin AS (
  UPDATE requests r SET final_status = :request_terminal
    FROM req WHERE r.id = req.id AND r.created_at = req.created_at
  RETURNING r.id
)
SELECT count(*) AS closed FROM fin;                   -- 应用层断言 = 1，否则 ROLLBACK
-- ⚠️ 块内不写 COMMIT：同上
```

- **ⓒ 是"条件执行"而非"闸门"**：K4 时 reservation 已 `settled`，`resv`/`agg` 返回 0 行是**正常的**，不影响 ⓔ 关单。
- **金额不重复计入**：`agg` 只在本次真正发生 `reserved → settled` 跃迁时才动聚合，与 `finalize_upstream` 天然互斥。

**`finalize_recovery` 是唯一按 request 批量终结的入口**，且必须**分跳给不同终态**（本轮自查发现）：

> 崩溃时一个 request 可能留下**多个**非终态 attempt——例如 hop1 被接管取消但其同步终态写入恰好没落、hop2 正在执行。若像上方骨架那样对本跳定终态，会漏掉 hop1；若按 `request_id` 一律写成同一个终态，又会把 hop1 误记成 hop2 的结果。

```sql
-- ⓐ 非最后一跳的非终态 attempt → 必然是被接管取消的（否则不会有下一跳）
UPDATE attempts SET attempt_status = 'canceled_by_sla',
       cancel_reason = COALESCE(cancel_reason,'sla_takeover'),
       canceled_by_sla = true, ended_at = now()
 WHERE request_id = :rid AND request_created_at = :rcat
   AND attempt_status IN ('pending','committed')
   AND attempt_no < :last_attempt_no;
-- ⓑ 最后一跳 → 按 §4.2bis 分流表（①/②/③b1/③b2/③c）定终态
UPDATE attempts SET attempt_status = :attempt_terminal, ended_at = now()
 WHERE id = :last_attempt_id AND attempt_status IN ('pending','committed');
-- ⓒ 释放该 request 名下**全部**残留 claim —— **三张表**（第 33 轮修正：此段是
--    finalize_recovery 的重复示例，仍只放 canary，与上方统一释放段冲突）。
--    实现直接复用 finalize_recovery 主体的 rel_cap/rel_can/rel_prb 三段，本处不重复给。
```

- ⓐ 的归因是**保守且可辩护**的：能产生下一跳，说明本跳当时确实被判定为需要接管。
- 其费用按下方汇总规则计入（"已终结但无用量 → 该跳单跳保守估算"）。
- **验收**：[AC-35](./14-acceptance-matrix.md) 须含**多跳崩溃用例**——hop1 接管后 hop2 执行中 kill，恢复后断言 hop1 = `canceled_by_sla`（**不得**被写成 hop2 的终态）、hop2 按分流表、两跳的 canary claim 全部释放且 `canary_inflight` 归零。

**恢复结算金额 = 该 request 所有 attempt 的费用汇总**（第 10 轮 [critical]）：

> ⚠️ 上一版写"`actual_usd` = 单跳保守估算"——但**预留是 request 级、按 RoutePlan 全部跳求和的**。若首跳超时被取消（钱已真实花掉）、第二跳崩溃，只结算单跳就会把首跳成本整个漏掉；而 `finalize` 又会一次性释放**整笔**预留 → `settled_usd` 系统性偏低，调用方可持续突破日费用上限。

```
actual_usd(request) = Σ over 该 request 的所有 attempt:
   ├─ 已终结且有实际用量（attempt_usage 有行） → 用**实际费用**
   ├─ 已终结但无用量（canceled_by_sla / failed 且上游已发出） → 用该跳**单跳保守估算**
   ├─ 已终结且确定未计费（external_call_started_at IS NULL）  → 0
   └─ 本次悬挂的那一跳                                        → 该跳**单跳保守估算**
```

- 该汇总**在 `finalize` 的同一事务内计算并写入**，与预留释放原子完成。
- **验收**：[AC-35](./14-acceptance-matrix.md) 与 [AC-33](./14-acceptance-matrix.md) 须有**联合用例**：首跳接管后第二跳崩溃，恢复后 `actual_usd` **必须包含首跳的实际成本**，且 `reserved_usd` 归零、`settled_usd` 不偏低。

**三条不变式**：

1. **无非终态残留**：扫描后该 request 的所有 attempt 与 request 自身都不再处于 `pending`/`committed`。
2. **无配额泄漏**：`client_reservations` 不再是 `reserved`，`client_daily_spend.reserved_usd` 已扣回该请求的 `estimated_usd`。②③ 记为已花费（保守），而非挂在预留上——**挂在预留 = 永久占用配额**。
3. **两套统计口径必须分开算**（对抗性审查第 8 轮 [high]：上一版一刀切"不计入成功率"，把**我们自己崩溃造成的用户可见中断**从 SLA 分母里抹掉了，与 FR-071「用户真实经历的失败和流中断始终计入 SLA」、AC-12「首字后中断记为完整失败」直接冲突，会让崩溃在 SLA 面板上完全隐形）：

   | 口径 | 用途 | `unknown_billing` | `interrupted` |
   | --- | --- | --- | --- |
   | **用户侧 SLA**（按 `requests` 算） | 有效可用性、错误预算、告警 | **计入失败**（`final_status='failed'`） | **计入失败**；是否计入 `stream_break_rate` 取决于**是否已确认写出过内容**（见下） |
   | **渠道健康**（按 `attempts` 算，喂 selector） | 冷却、样本门槛、排序 | **不计入**该 binding 成功率 | **不计入**该 binding 成功率 |
   | 成本 | 费用统计、配额 | 计入（估算值） | 计入（估算值） |
   | TTFT | 首字统计 | 无 TTFT（未见首字） | **已记录的 `content_aware_ttft_ms` 照常计入**（那是真实观测值，用户真的等到了首字） |

   > **为什么两套口径方向相反**：进程崩溃是**我们的**故障，不是渠道的故障。计入用户 SLA 是诚实（用户确实失败了）；不计入渠道健康是准确（否则会冤枉一个健康渠道、把它冷却掉，故障范围反而扩大）。
   > **实现**：`resource_health` 的成功率/样本计数按 `attempt_status NOT IN ('unknown_billing','interrupted','canceled_by_client')` 过滤；SLA 聚合按 `requests.final_status` 算，**不过滤**。
   >
   > **`stream_break_rate` 的唯一判据**：
   > ```
   > 分子（计入流中断）⟺
   >      attempts.stream_broken = true                          -- 上游断流，我们观测到了
   >   OR ( requests.final_status = 'interrupted'                 -- 我们崩了
   >        AND attempts.downstream_first_byte_written_at IS NOT NULL )  -- 且已确认写出过内容
   > 分母 = 同窗口内 requests.is_streaming = true 的全部请求
   > ```
   > ⚠️ **第 13 轮冻结的版本是错的**（第 15 轮 [critical]）：当时只写了
   > `stream_broken = true OR downstream_first_byte_written_at IS NOT NULL`——
   > 后半句对**每一个正常成功的流式请求**都成立，等于把全部成功流算成中断，指标直接失去意义。
   > 漏掉的是「**才中断**」那一半：必须同时要求结果是中断/失败，不能只看"写出过内容"。
   >
   > 逐场景核对：正常成功（`completed`）**不计** ✓；K2（`failed`）**不计** ✓；
   > K3／K4（`interrupted` 且首字节已写出）**计** ✓；上游断流（`stream_broken=true`）**计** ✓。

> ②③b 的 `actual_usd` 含估算成分，故 `needs_manual_review=true`；运维据告警核对上游账单后走 `POST /admin/reservations/{request_id}/adjust`——它是[§2bis 的**独立 `adjust` 事务**](#2bis-网关调用方凭证域入站鉴权对抗性审查新增)（从 `settled` 出发、`FOR UPDATE` 锁定、按差额修正），**不是 `finalize`**：`finalize` 的闸门是 `state='reserved'`，对已 `settled` 的待核对行必然影响 0 行、静默失效。幂等键用 `reservation_adjustments.event_key`，**不是** `settle_event_key`。**禁止直接改聚合表**。

**验收**：[AC-35](./14-acceptance-matrix.md) 逐一断言四个崩溃时点的 `attempt_status` + `final_status` + `client_reservations.state` + `client_daily_spend.reserved_usd` **四项全部终结**。

```sql
-- 恢复扫描的两个索引（request 级入口 + attempt 侧租约判活）
CREATE INDEX idx_requests_open ON requests(created_at) WHERE final_status = 'pending';
CREATE INDEX idx_attempts_lease ON attempts(request_id, attempt_no, lease_heartbeat_at NULLS FIRST);
-- ⚠️ 支撑本扫描的 `idx_outbox_undelivered` 建在 §9.2bis `ledger_outbox` 定义处——
--    索引不能早于表，迁移按文档顺序抽取时会直接失败（第 18 轮）。
```

### 4.3 逐 Attempt 用量与费用

```sql
-- attempt 级用量/费用（FR-058、FR-016/019；假设4 token+cost 已收口）
-- 一期 upstream_seq 恒为 1（我们不发隐藏重试，FR-119）；保留该列仅为二期若接入会隐藏重试的上游时零改表（§11 开放点5）
CREATE TABLE attempt_usage (
  id                UUID NOT NULL,            -- UUIDv7（分区表主键须含分区键，见表尾复合 PK）
  attempt_id        UUID NOT NULL,
  request_created_at TIMESTAMPTZ NOT NULL,    -- 冗余分区键
  upstream_seq      SMALLINT NOT NULL DEFAULT 1, -- 一期恒为 1；>1 保留给二期（FR-119）
  prompt_tokens     INTEGER,                  -- 上游终帧 usage.prompt_tokens（旁路观察提取，[03 §7](./03-upstream-layer.md)）
  completion_tokens INTEGER,                  -- usage.completion_tokens
  total_tokens      INTEGER,                  -- usage.total_tokens
  prompt_cached_tokens INTEGER,               -- usage.prompt_tokens_details.cached_tokens（缓存命中部分，FR-054）
  -- FR-056 预测需要**分母**：命中率 = prompt_cached_tokens / cacheable_prompt_tokens。
  -- 只有 prompt_tokens 不够——系统提示、工具定义等固定前缀才可缓存，用户新增的那一轮不可缓存，
  -- 拿 prompt_tokens 当分母会系统性低估命中率（开发视角审查第 27 轮 [P1]）。
  cacheable_prompt_tokens INTEGER,            -- 本次请求中**理论可缓存**的输入 token 数
  total_cost        nonneg_usd,               -- 该跳实际费用：上游若回传费用则用它，否则按价格版本自算（见 cost_source）
  cost_items        JSONB,                    -- 费用明细（元数据；输入/输出/缓存分项，便于排障与对账）
  cost_source       TEXT NOT NULL DEFAULT 'upstream' CHECK (cost_source IN ('upstream','estimated')),
  -- 预估 vs 实际扣费差异（FR-016/019）：超容差标计费异常
  estimated_cost    nonneg_usd,
  cost_variance     usd_amount,               -- 实际-预估；**可为负**（实际低于预估），故用可正负的 usd_amount（FR-019/AC-23）
  PRIMARY KEY (id, request_created_at),       -- 分区表：主键必须包含分区键
  -- 指向 attempts 的复合外键（分区表间引用须带分区键）
  FOREIGN KEY (attempt_id, request_created_at)
      REFERENCES attempts (id, request_created_at),
  -- 支撑 finalize_upstream 的 ON CONFLICT 幂等目标（第 19 轮 [critical]）；须含分区键
  UNIQUE (attempt_id, request_created_at, upstream_seq)
) PARTITION BY RANGE (request_created_at);
```

> **不存正文**：`decision_snapshot` / `cost_items` / `error_message` 均为元数据 JSON / 文本，不含 messages / prompt 正文 / 请求头体（FR-112）。失败/取消请求 `attempt_usage` 可为空（假设4：mock-500 usageLogs 为空，符合预期）。

### 4.4 连续会话前缀首字（FR-050/051）

```sql
-- 会话前缀首字账目（FR-050/051、8.1；AC-06/07）：每轮判断前缀平均是否达标
CREATE TABLE session_prefix_ledger (
  session_id        TEXT NOT NULL,
  turn_no           INTEGER NOT NULL,
  request_id        UUID NOT NULL,
  turn_first_token_ms INTEGER,               -- 本轮实际首字（自算，AC-31）
  cumulative_first_token_ms BIGINT,          -- 累计首字
  prefix_avg_ms     INTEGER,                 -- 前缀平均（=累计/轮数）
  target_ms         INTEGER,                 -- 本轮目标（默认前缀均 ≤10s，§11）
  met_target        BOOLEAN,                 -- 每轮结束达标判定（FR-051）
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (session_id, turn_no)
);
```

### 4.5 会话标识来源（多源提取，**不要求调用方配合**）

> 收口 §11 开放点 2。原则：**主流编码客户端本来就带会话标识**，网关只需按优先级"捞"，不新增对外契约要求；捞不到才退化，**绝不编造会话身份**。

**调研证据（2026-07-23/25，覆盖主流客户端与协议）**：

| 客户端 / 协议 | 客户端说的入站协议 | 自带会话标识 | 载体 | 一期可直连？（FR-111） |
| --- | --- | --- | --- | --- |
| **Claude Code**（v2.1.86+） | Anthropic Messages | ✅ 自动发送 | 头 `X-Claude-Code-Session-Id`（社区明确为**代理可观测性**设计：只读头、不碰 body） | ❌ **一期不收 Anthropic 入站** |
| **Codex CLI** | OpenAI Responses | ✅ 自动发送 | 头 `session_id` + `conversation_id`；body `prompt_cache_key`（其缓存键优先级 `session_id > conversation_id > user_id`） | ✅ |
| **OpenCode** | OpenAI CC（base_url 可配） | ❌ 当前不发 | 内部有 SessionID/ParentSessionID 但从不出网（[issue #12930](https://github.com/anomalyco/opencode/issues/12930) 请求中） | ✅（落序 7 退化） |
| **Gemini CLI** | Gemini API | ⚠️ 内部有不外发 | sessionId 存 `~/.gemini/tmp/<hash>/chats/`；协议侧有 `previous_interaction_id`（服务端会话态）。[#8944](https://github.com/google-gemini/gemini-cli/issues/8944)/[#13823](https://github.com/google-gemini/gemini-cli/issues/13823) 请求暴露 | ❌ **一期不收 Gemini 入站** |
| **Cline** | OpenAI CC（base_url 可配） | ❌ **不发**（源码级确认） | 会话标识按 **provider 白名单**下发：`cline`/`cline-pass` 发 `X-Task-ID` 头、`openai-codex` 发 `session_id` 头、`openrouter` 发 **JSON body** `session_id`；**通用 `openai-compatible` 不在名单内**，仅透传用户自配 `config.headers`（`sdk/packages/llms/src/providers/request-headers.ts` 的 `resolveRequiredProviderHeaders()` 对其返回 `undefined`） | ✅（落序 7 退化）**一期不适配** |
| OpenAI **Chat Completions** 协议 | —— | ⚠️ 无原生会话字段 | 仅 `user`（用户标识，**非会话**）；`prompt_cache_key` 可选 | ✅ |
| OpenAI **Responses** 协议 | —— | ✅ 协议原生 | `conversation`（持久 ID）、`previous_response_id`（逐轮链式） | ✅ |

> **协议范围已定（2026-07-23，收口 §11 开放点 6）**：**主力客户端为 Codex CLI，说的正是 OpenAI Responses**，已在一期 FR-111 范围内 → **维持 CC + Responses，不扩协议**。AxonHub 虽入站支持 4 种，Claude Code（Anthropic Messages）与 Gemini CLI（Gemini API）一期不直连；其提取规则保留在上表，仅为将来扩协议时零改动。
>
> **主力路径 = 序 2（Codex 的 `session_id`/`conversation_id` 头）+ 序 3（`prompt_cache_key`）**，二者都稳定且 Codex 自动发送 —— 一期会话标识**在主力场景下是可靠可得的**，序 7 退化只在非主力客户端（如 OpenCode）出现。

**提取优先级（protocol 层实现，命中即停）**：

| 序 | 来源 | 位置 | 稳定性 |
| --- | --- | --- | --- |
| 1 | `X-Claude-Code-Session-Id` | 头 | 稳定，整会话不变 |
| 2 | `session_id` → `conversation_id` | 头 | 稳定 |
| 3 | `prompt_cache_key` | body（OpenAI 官方缓存亲和键） | 稳定；**同时喂缓存作用域**（FR-055） |
| 4 | `conversation` | body（Responses） | 稳定 |
| 5 | `previous_response_id` | body（Responses） | **逐轮变化**——须维护 `previous_response_id → session_id` 链映射才能还原会话 |
| 6 | `X-Session-Id` | 头 | 通用回退，供自定义客户端**可选**使用 |
| 7 | 全未命中 | —— | **按单轮请求处理**：不做前缀首字统计（FR-050/051 跳过该请求），不派生、不编造 |

**约束**：

- ⚠️ **序 3/5 在"经 AxonHub 的响应"中不可用**（[07 §3bis](./07-axonhub-runtime-probes.md) 真实上游实测）：AxonHub 的 Responses round-trip 把顶层字段从 35 砍到 7，`prompt_cache_key` 与 `previous_response_id` **均被丢弃**。
  - **对请求侧提取无影响**：序 3/5 提取的是**调用方发来的请求体**，由我方 `protocol` 层在转发前读取，不经过 AxonHub。
  - **响应侧回写现已可用**：转向自研后走**字节级透传**（[03 §1](./03-upstream-layer.md)），上游响应的 `prompt_cache_key`/`previous_response_id` 原样保留，可从旁路观察中回收记入缓存作用域。
  - **主力路径不受影响**：Codex 走序 1/2 的 **HTTP 头**，AxonHub 不改头部。
- 序 3~5 在 body 中，需解析请求体——**FR-112 禁止的是"存储"正文，不禁止读取**；提取后只落 `requests.session_id` 这一个标识值，正文不入库。头部来源（序 1/2/6）无需碰 body，优先级更高也更省。
- 序 7 的退化是**有意的**：宁可少统计一条前缀首字，也不能用"IP+Key+模型"这类拼接键把并发的不同会话错误归并（会让 FR-050/051 的前缀平均失真）。
- OpenCode 类客户端当前落到序 7；待其 #12930 落地后自动升到序 2，**无需我方改动**。
- **Cline（一期不适配，仅留调研结论）**：源码确认它以 **provider 白名单**决定是否下发会话标识，通用 `openai-compatible` 路径**不发**任何会话头，故接入我方网关时落序 7（按单轮处理）。若将来需要支持，有两条零改动路径：① 用户在 Cline 的 OpenAI-compatible 配置里**自填自定义头**（其 `config.headers` 会原样透传，可填 `X-Session-Id` 命中我方序 6）；② Cline 官方把通用 provider 纳入白名单。**一期不为其做任何适配**（主力为 Codex）。

**索引**

```sql
CREATE INDEX idx_attempts_request   ON attempts(request_id, request_created_at); -- 归集同一请求的多跳
CREATE INDEX idx_attempts_binding   ON attempts(binding_id, started_at);         -- 喂健康统计（§7）
CREATE INDEX idx_attempts_upresp    ON attempts(upstream_response_id);           -- 排障关联上游响应
CREATE INDEX idx_attempts_cancel    ON attempts(cancel_reason)
                                       WHERE cancel_reason<>'none';               -- 取消口径统计（AC-30）
CREATE INDEX idx_usage_attempt      ON attempt_usage(attempt_id, request_created_at);
CREATE INDEX idx_req_session        ON requests(session_id, created_at) WHERE session_id IS NOT NULL;
CREATE INDEX idx_req_tenant_level   ON requests(tenant_id, sla_level, created_at); -- 指标维度（FR-091）
```

**服务 FR/AC**：FR-040、FR-050/051、FR-058、FR-070/071/072、FR-076/078/079/080、FR-092、FR-097/098/099、FR-112、FR-116、FR-119；AC-06/07/09/12/13/16、AC-30（errorMessage 归并）、AC-31（TTFT 自算 vs 存证）、AC-32（取消传播止损）。

---

## 5. 订阅台账域（双倍率 + 共享额度 + 三家族真实字段）

> ⏭ **本域已移入二期**（[15 §1.2](./15-scope-and-preflight.md)）：**表结构保留**（空表无成本、便于二期直接启用），一期**不写入、不参与调度**。

> FR-033~039。字段以 **ISSUE-002 §3.2 sub2api ent schema** 与 **§3.3 ASXS `/api/me/billing/state`** 真实结构为准。一期**不建模签到额度**（FR-034 已删该句；NewAPI 系接受额度预测偏低）。双倍率（用满/实际，参数14/AC-24）。共享额度按 `(user_id, group_id)` 聚合（FR-035，源码级确证）。可主动重置额度只读登记、不自动触发。

### 5.1 订阅计划（可售套餐 / 商品）

```sql
-- 订阅计划（FR-033）：对应 sub2api subscription_plans + group / ASXS purchase products
CREATE TABLE subscription_plans (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  channel_id      BIGINT NOT NULL REFERENCES channels(id),
  external_plan_id TEXT,                      -- 上游 planId（ASXS）/ group_id（sub2api）
  name            TEXT NOT NULL,              -- ASXS planName / sub2api plan.name（如「每日90刀」）
  -- 固定费用（FR-033）：sub2api plans.price / ASXS products.priceCnyCent，归一为美元
  fixed_fee       nonneg_usd NOT NULL,
  currency        TEXT NOT NULL DEFAULT 'USD',-- 一期仅名称（FR-018）
  -- 有效期（FR-033）：sub2api validity_days×unit / ASXS durationDays
  validity_days   INTEGER,
  billing_period  TEXT,                       -- daily/weekly/monthly（ASXS limits.limitType + windowMode=fixed）
  -- 周期包含额度（FR-033）：ASXS limits.limitMicros / sub2api group.*_limit_usd
  period_quota    nonneg_usd,
  supported_models JSONB,                     -- 支持模型（FR-033）
  -- 倍率（FR-033）：sub2api group.rate_multiplier + 高峰倍率
  rate_multiplier NUMERIC(12,6),
  peak_rate_enabled BOOLEAN DEFAULT false,    -- sub2api group.peak_rate_enabled
  peak_start      TEXT,                       -- peak_start（时段）
  peak_end        TEXT,
  peak_rate_multiplier NUMERIC(12,6),
  reset_rule      TEXT,                       -- 重置规则（FR-033）：固定窗口 日/周/月（sub2api window / ASXS fixedResetTime）
  renewal_status  TEXT,                       -- 续订状态（FR-033）：ASXS renewalRule / renewAllowed
  -- 超额计费规则（FR-033）：sub2api 无超额概念 → 据实登记 'no_overage_block'（ISSUE-002 §3.2 结论2）
  overage_rule    TEXT NOT NULL DEFAULT 'unknown'
                    CHECK (overage_rule IN ('no_overage_block','metered','unknown')),

  -- ── 双倍率（参数14/AC-24/FR-057/058）──
  usable_multiplier NUMERIC(12,6),            -- 用满倍率＝固定费用÷周期额度×分组倍率 → 调度排序
  actual_multiplier NUMERIC(12,6),            -- 实际倍率＝固定费用÷实际消耗×倍率 → 账务报表

  -- ── 额度来源优先级（FR-033、术语§3；ASXS primarySource/secondarySource）──
  primary_source  TEXT,                       -- 如 'subscription'
  secondary_source TEXT,                      -- 如 'balance' —— 判断订阅额度是否真会被消耗

  -- ── 可主动重置额度（FR-033；ASXS dailyReset）：只读登记，不自动触发 ──
  active_reset_supported BOOLEAN NOT NULL DEFAULT false,
  active_reset_threshold_pct NUMERIC(5,2),    -- usageThresholdPercent（如 90）
  active_reset_daily_limit INTEGER,           -- dailyLimit（如 4 次/日）

  data_source     TEXT NOT NULL,              -- auto_collect / manual（FR-011）
  fetched_at      TIMESTAMPTZ NOT NULL,
  valid_until     TIMESTAMPTZ,                -- 人工录入 7 天有效（FR-011）
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

### 5.2 已购订阅实例（含日/周/月窗口用量）

```sql
-- 已购订阅（FR-033/034/036）：对应 sub2api user_subscriptions / ASXS billing/state
CREATE TABLE user_subscriptions (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  plan_id         BIGINT NOT NULL REFERENCES subscription_plans(id),
  channel_id      BIGINT NOT NULL REFERENCES channels(id),
  -- 共享额度聚合键（FR-035，源码级确证 UpdateSubscriptionUsage(userID,groupID,cost)）
  ext_user_id     TEXT NOT NULL,              -- sub2api user_id
  group_id        TEXT NOT NULL,              -- sub2api group_id —— 共享额度按 (user_id,group_id) 归集，不按 Key
  starts_at       TIMESTAMPTZ,                -- sub2api starts_at / ASXS startsAt
  expires_at      TIMESTAMPTZ,               -- expires_at / expiresAt（到期时间，FR-033）
  status          TEXT NOT NULL CHECK (status IN ('not_effective','active','expired','suspended','data_unknown')),
                                              -- 对齐 PRD §9.3 订阅状态 + sub2api active/expired/suspended
  -- 已用/剩余（FR-034）：区分订阅额度、现金余额、超额付费
  used_quota      nonneg_usd,                 -- ASXS usedMicros/1e6
  left_quota      nonneg_usd,                 -- ASXS leftMicros/1e6
  remaining_days  INTEGER,                    -- ASXS remainingDays（到期紧迫度，FR-037）
  data_source     TEXT NOT NULL,
  fetched_at      TIMESTAMPTZ NOT NULL,
  valid_until     TIMESTAMPTZ,                -- 人工 7 天有效（FR-011）
  UNIQUE (channel_id, ext_user_id, group_id)  -- 共享额度只计一次（FR-035/AC-22）
);

-- 订阅周期窗口用量（FR-034/036）：sub2api daily/weekly/monthly window_start + usage_usd
-- 拆表表达三窗口，避免宽表；也承接 user_platform_quota 视图
CREATE TABLE subscription_quota_windows (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  subscription_id BIGINT NOT NULL REFERENCES user_subscriptions(id),
  window_kind     TEXT NOT NULL CHECK (window_kind IN ('daily','weekly','monthly')),
  limit_usd       nonneg_usd,                 -- *_limit_usd（周期上限）
  usage_usd       nonneg_usd,                 -- *_usage_usd（已用）
  window_start    TIMESTAMPTZ,                -- *_window_start
  window_resets_at TIMESTAMPTZ,               -- *_window_resets_at（重置时间 → FR-036 到期未用预测）
  fetched_at      TIMESTAMPTZ NOT NULL,
  UNIQUE (subscription_id, window_kind)
);

-- 到期未用额度预测（FR-036/039、8.5；AC-20/21/23）
CREATE TABLE subscription_waste_forecast (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  subscription_id BIGINT NOT NULL REFERENCES user_subscriptions(id),
  predicted_wasted_quota nonneg_usd,          -- max(0, 剩余 - 到期前预计合格消耗)
  eligible_demand nonneg_usd,                 -- 合格业务需求预估
  recent_consumption_rate nonneg_usd,         -- 近期消耗速度
  active_reset_gain nonneg_usd,               -- 可主动重置带来的外生额度（FR-036 外生变量）
  unusable_reason TEXT,                       -- 不可利用原因（FR-039/AC-21：倍率/性能/稳定性不达标）
  forecast_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

**索引**

```sql
CREATE INDEX idx_plans_channel      ON subscription_plans(channel_id);
CREATE INDEX idx_subs_shared        ON user_subscriptions(ext_user_id, group_id); -- 共享额度聚合（FR-035）
CREATE INDEX idx_subs_expiry        ON user_subscriptions(expires_at) WHERE status='active'; -- 临近到期扫描（FR-037）
CREATE INDEX idx_qwin_reset         ON subscription_quota_windows(window_resets_at);
```

**服务 FR/AC**：FR-011、FR-033/034/035/036/037/038/039、FR-057/058；AC-20/21/22/23/24（双倍率）、AC-28（三家族采集）。
**一期不建模**：签到额度（FR-034 删句；NewAPI 系 `checkin` 不入账，接受额度预测偏低）；超额计费对 sub2api 系登记 `no_overage_block`（ISSUE-002 §3.2 结论2）。

---

## 6. 渠道健康 / 冷却 / 样本门槛域

> 参数11：近 1h≥20 次或近 24h≥100 次可作主渠道；观察期 30min 或连续 50 次成功；冷却 5min 起翻倍至 4h。全部可配置（阈值走 §2 `config_params`，本域只存实时状态与计数）。健康状态六态（FR-046）；余额状态五态（PRD §9.2）。

```sql
-- 资源健康状态（FR-042/043/046/047；参数11）：逐 binding 维护
CREATE TABLE resource_health (
  binding_id      BIGINT PRIMARY KEY REFERENCES bindings(id),
  -- 健康六态（FR-046）
  health_state    TEXT NOT NULL DEFAULT 'unknown'
                    CHECK (health_state IN ('unknown','canary','observing','available','degraded','cooling','disabled')),
                    -- ⚠️ canary（一期受控验证态，FR-121，[05 §2.0](./05-scheduling-and-operations.md)）：
                    --    新登记 / 冷却期满的 binding 在此态下按硬上限承接**真实业务流量**积累首批样本。
                    --    没有它，unknown 渠道因 low_confidence 永不作主渠道 → 永远拿不到样本 →
                    --    只能在主渠道故障时首次上生产（对抗性审查第 7 轮 [high]）。
  -- 样本门槛计数（参数11）：低样本/过期标低可信（FR-043）
  sample_count_1h  INTEGER NOT NULL DEFAULT 0,   -- 近 1h 样本（≥20 可作主渠道）
  sample_count_24h INTEGER NOT NULL DEFAULT 0,   -- 近 24h 样本（≥100 可作主渠道）
  last_sample_at   TIMESTAMPTZ,                  -- 样本过期判定（FR-043）
  low_confidence   BOOLEAN NOT NULL DEFAULT true,-- 样本过少/过期 → 不作稳定主渠道（FR-043）

  -- 近期分位指标（FR-042）：不用标称值/总体平均
  p50_ttft_ms     INTEGER,
  p95_ttft_ms     INTEGER,
  p99_ttft_ms     INTEGER,
  success_rate    NUMERIC(5,4),                  -- 完整成功率
  stream_break_rate NUMERIC(5,4),

  -- 冷却（参数11）：5min 起翻倍至 4h；连续失败延长（FR-065）
  cooldown_until   TIMESTAMPTZ,
  cooldown_current_sec INTEGER,                  -- 当前冷却时长（300 起，×2 上限 14400）
  consecutive_failures INTEGER NOT NULL DEFAULT 0,

  -- 观察期恢复（FR-047）：30min 或连续 50 次成功后转可用
  observing_since  TIMESTAMPTZ,
  observing_success_count INTEGER NOT NULL DEFAULT 0,

  -- 容量占用（FR-029/032，[05 §4.3](./05-scheduling-and-operations.md)）：
  -- 仅对**已登记容量**的 binding 维护；未登记者**整段跳过占用事务**，这三列恒为 0（见 §6bis-2）。
  rpm_window_start    TIMESTAMPTZ,              -- 分钟窗口起点
  rpm_used            INTEGER NOT NULL DEFAULT 0,
  concurrency_inflight INTEGER NOT NULL DEFAULT 0,

  -- 一期受控验证（canary，[05 §2.0](./05-scheduling-and-operations.md)）
  canary_since        TIMESTAMPTZ,               -- 进入 canary 的时刻
  canary_window_start TIMESTAMPTZ,               -- 当前小时窗口起点（配额按小时重置）
  canary_used_in_window INTEGER NOT NULL DEFAULT 0, -- 本窗口已分配的 canary 请求数（硬上限）
  canary_inflight     SMALLINT NOT NULL DEFAULT 0,  -- 并发闸（默认上限 1）
  canary_failures     SMALLINT NOT NULL DEFAULT 0,  -- 连续失败数，达阈值退回 cooling

  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 分窗健康快照（FR-041）：按模型/上下文区间/请求类型/时段/资源分别统计
-- 供 P50/P95/P99 与成功率的滚动重算；作为 resource_health 汇总来源
CREATE TABLE health_metric_windows (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  binding_id      BIGINT NOT NULL REFERENCES bindings(id),
  model_id        BIGINT REFERENCES models(id),
  request_type    TEXT,
  context_bucket  TEXT,                          -- 上下文区间（FR-041）
  window_start    TIMESTAMPTZ NOT NULL,
  window_kind     TEXT NOT NULL CHECK (window_kind IN ('1h','24h')),
  sample_count    INTEGER NOT NULL DEFAULT 0,
  p50_ttft_ms     INTEGER,
  p95_ttft_ms     INTEGER,
  p99_ttft_ms     INTEGER,
  success_count   INTEGER,
  stream_break_count INTEGER,
  -- ⚠️ 同上：model_id/request_type/context_bucket 可空，普通 UNIQUE 挡不住重复行 →
  --    同一窗口的「总体统计」可写多份，selector 读到的健康度不确定（第 19 轮 [high]）
  UNIQUE NULLS NOT DISTINCT (binding_id, model_id, request_type, context_bucket, window_kind, window_start)
);

-- 质量事件（FR-007）：空响应/格式破坏/能力不符/疑似模型替换（P1）
CREATE TABLE quality_events (
  id            UUID PRIMARY KEY,
  binding_id    BIGINT NOT NULL REFERENCES bindings(id),
  request_id    UUID,
  event_type    TEXT NOT NULL CHECK (event_type IN ('empty_response','format_broken','capability_mismatch','suspected_swap')),
  detected_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

**索引**

```sql
CREATE INDEX idx_health_state    ON resource_health(health_state);
CREATE INDEX idx_health_cooldown ON resource_health(cooldown_until) WHERE health_state='cooling';
CREATE INDEX idx_hwin_binding    ON health_metric_windows(binding_id, window_kind, window_start DESC);
```

**服务 FR/AC**：FR-007、FR-040~047、FR-060/065/066；AC-08/10。

---

### 6quater. 缓存命中率滚动统计（FR-056，一期）

> ⚠️ **FR-056 此前只有需求文字没有算法**（开发视角审查第 27 轮 [P1]）：设计要求"预测切换后缓存命中率，低于目标则限制切换"，但**用什么数据算、公式是什么、目标值从哪来**三样都没定义 —— 开发只能自己发明，而发明出来的东西直接决定长会话省不省钱。

```sql
-- 会话 × 缓存作用域 的滚动命中率（供 selector 预测切换代价）
CREATE TABLE cache_hit_windows (
  session_id      TEXT NOT NULL,
  cache_scope_id  BIGINT NOT NULL REFERENCES cache_scopes(id),
  window_start    TIMESTAMPTZ NOT NULL,        -- 小时窗口
  cached_tokens   BIGINT NOT NULL DEFAULT 0,   -- Σ prompt_cached_tokens
  cacheable_tokens BIGINT NOT NULL DEFAULT 0,  -- Σ cacheable_prompt_tokens（分母）
  turn_count      INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (session_id, cache_scope_id, window_start)
);
```

**窗口谁写、何时写**（第 29 轮 [P0]：此前只有表和公式，没说写入点，开发无从落地）：

| 项 | 冻结做法 |
| --- | --- |
| 写入点 | **`finalize_upstream` 事务内**（与 `attempt_usage` 同一条链）—— 那时 usage 刚拿到，`prompt_cached_tokens` 与 `cacheable_prompt_tokens` 都已知 |
| 纳入哪些 attempt | 仅 `attempt_status='completed'` 且 `cost_source='upstream'`（真实 usage）。**取消跳、失败跳、估算用量一律不计** —— 它们的缓存数据要么不存在要么不可信 |
| `cacheable_prompt_tokens` 怎么算 | `UsageSnapshot` 由上游回传时直接取；**上游不回传时**按 `prompt_tokens − 本轮新增 token 数`估算，本轮新增按请求体增量字节 / 2 取上界；仍无法估算则**该次不写窗口**（宁可少一个样本，不可写入错误分母） |
| 窗口粒度 | `(session_id, cache_scope_id, date_trunc('hour', now()))`，`ON CONFLICT DO UPDATE` 累加 |
| 无 `session_id` 的请求 | **不写**（单轮请求没有前缀可延续，纳入会稀释长会话的统计） |

```sql
-- 在 finalize_upstream 链尾追加（仅当 attempt 完成且 usage 为真实值）
INSERT INTO cache_hit_windows (session_id, cache_scope_id, window_start,
                               cached_tokens, cacheable_tokens, turn_count)
SELECT :session_id, :cache_scope_id, date_trunc('hour', now()), :cached, :cacheable, 1
 WHERE :session_id IS NOT NULL AND :cost_source = 'upstream' AND :cacheable > 0
ON CONFLICT (session_id, cache_scope_id, window_start) DO UPDATE
   SET cached_tokens    = cache_hit_windows.cached_tokens    + EXCLUDED.cached_tokens,
       cacheable_tokens = cache_hit_windows.cacheable_tokens + EXCLUDED.cacheable_tokens,
       turn_count       = cache_hit_windows.turn_count       + 1;
```

**预测公式**（`selector` 排序前，纯内存快照，不查库）：

```text
PredictCacheHitRate(session, candidate_binding):
  cur_scope  = 该 session 上一轮命中的 cache_scope_id
  cand_scope = candidate_binding 的 cache_scope_id

  ① cand_scope == cur_scope（作用域可延续）
       → 预测 = 该 (session, scope) 的近窗口实测命中率
                = cached_tokens / NULLIF(cacheable_tokens, 0)
       → 无历史（首轮）时取该 scope 的全局中位数
  ② cand_scope != cur_scope（换作用域，前缀作废）
       → 预测 = 0（本轮必然全量重算）
         ——**下一轮**起才会重新积累，故切换代价 ≈ 本轮 cacheable_tokens × 输入单价

  目标值 target = config_params['cache_switch_min_hit_rate']   -- 默认 0.6，全局单值
  抑制条件：预测 < target AND 不存在"更高等级 SLA 要求接管"的理由
```

- **切换代价入决策快照**：`decision_snapshot.excluded[]` 记 `{binding, reason:'cache_switch_loss', predicted, target, est_extra_cost_usd}` —— [AC-07](./14-acceptance-matrix.md) 要断言这三个数**真的被算出来了**，而不是只有一个布尔。
- **目标值只有一个来源：`config_params['cache_switch_min_hit_rate']`（默认 0.6）**。
  > ⚠️ 这里刻意**不放进 `sla_targets`**：`sla_targets` 承载的是**对外 SLA 承诺**，而缓存命中率一期**不对外承诺**（≥90% 的承诺推二期，[PRD §11](../PRD.md)）。
  > 它只是**内部的切换抑制阈值** —— 混进 `sla_targets` 会让人误以为我们承诺了缓存命中率，也会因为要按等级查而与「不读等级名」的决议冲突。
  > 0.6 是待实测的起点值，长会话场景跑一段后按 `/metrics` 的实际命中率调整（[06 §6](./06-deployment-and-operations.md)）。
- **为何分母是 `cacheable_prompt_tokens` 而非 `prompt_tokens`**：可缓存的只有固定前缀（系统提示、工具定义、历史轮次），用户本轮新增内容不可缓存。用 `prompt_tokens` 作分母会随会话变长而系统性低估命中率，越是长会话越失真 —— 而长会话正是本项目的主场景。
- **上游不回 `prompt_cached_tokens` 时**：该窗口不计入统计（`cacheable_tokens` 不累加），预测退回全局中位数；**不可**把缺失当成 0 命中，否则会误判所有该类渠道。

**服务 FR/AC**：FR-054/055/056；AC-07。

---

### 6bis-2. 容量占用（FR-029/032，AC-18 的执行载体）

> ⚠️ 第 30 轮 [P0]：[05 §4.3](./05-scheduling-and-operations.md) 只写了"用与 canary 同构的 DB 原子计数"，**没有表、没有 SQL、没有释放与恢复规则** —— AC-18 无从实现。

```sql
-- 每次占用一行，与 canary_claims / probe_claims 同构
CREATE TABLE capacity_claims (
  claim_id      UUID PRIMARY KEY,
  binding_id    BIGINT NOT NULL REFERENCES bindings(id),
  request_id    UUID NOT NULL,
  attempt_id    UUID NOT NULL,
  kind          TEXT NOT NULL CHECK (kind IN ('normal','committed','takeover','canary','probe')),
                -- ⚠️ 第 37 轮补 'canary'：文档说 kind 由 RoutePlanEntry.Role 推出，
                --    而 Role 含 canary，原枚举却没有 → CHECK 直接拒绝。
                --    canary 的天花板取 `capacity_ceiling_normal`（65%）——它是无承诺流量。
  lease_owner   TEXT NOT NULL,
  lease_expires_at TIMESTAMPTZ NOT NULL,
  state         TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active','released')),
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  released_at   TIMESTAMPTZ
);
CREATE INDEX idx_capacity_active ON capacity_claims(binding_id) WHERE state = 'active';
```

**各类流量的并发天花板**（比例来自 [05 §4.2](./05-scheduling-and-operations.md)，均可配）：

| `kind` | 可用上限 | 依据 |
| --- | --- | --- |
| `normal` | `limit × (1 − 承诺20% − 接管10% − 测活5%) = 65%` | 日常流量不得挤占三类保留 |
| `committed` | `limit × 85%`（65% + 承诺 20%） | 承诺流量可用自己的保留 |
| `takeover` | `limit × 95%`（+ 接管 10%） | 接管是 SLA 最后防线，天花板最高 |
| `probe` | `limit × 70%`（65% + 测活 5%） | 探索不得伤主流量 |
| `canary` | `limit × 65%`（同 normal） | canary 用的是本就要发的无承诺请求，不额外占保留 |

**原子占用**（与 dispatch 同事务，`kind` 由 `RoutePlanEntry.Role` 推出）：

```sql
WITH lim AS (
  SELECT b.id, b.concurrency_limit, b.rpm_limit FROM bindings b WHERE b.id = :bid
),
claimed AS (
  UPDATE resource_health h
     SET rpm_window_start = CASE WHEN h.rpm_window_start IS NULL
                                  OR h.rpm_window_start < date_trunc('minute', now())
                                 THEN date_trunc('minute', now()) ELSE h.rpm_window_start END,
         rpm_used = CASE WHEN h.rpm_window_start IS NULL
                          OR h.rpm_window_start < date_trunc('minute', now())
                         THEN 1 ELSE h.rpm_used + 1 END,
         concurrency_inflight = h.concurrency_inflight + 1
    FROM lim
   WHERE h.binding_id = lim.id
     AND (lim.concurrency_limit IS NULL                      -- 未登记 → 不设闸
          OR h.concurrency_inflight < floor(lim.concurrency_limit * :ceiling))
     AND (lim.rpm_limit IS NULL
          OR h.rpm_window_start IS NULL                       -- ⚠️ 首个窗口：NULL 必须显式放行，
                                                              --    否则 `<` 恒 UNKNOWN → 永远 0 行
          OR h.rpm_window_start < date_trunc('minute', now())
          OR h.rpm_used < floor(lim.rpm_limit * :ceiling))
  RETURNING h.binding_id)
INSERT INTO capacity_claims(claim_id, binding_id, request_id, attempt_id, kind,
                            lease_owner, lease_expires_at)
SELECT :claim_id, binding_id, :rid, :attempt_id, :kind, :owner, now() + interval '60 seconds'
  FROM claimed
RETURNING claim_id;
```

- **返回 0 行 = 该 binding 容量已满** → 该候选**排除出本次 RoutePlan**，selector 取下一个；全部候选都满 → 按 [§3.2](./05-scheduling-and-operations.md) 全资源不可用处置（按承诺与否决定排队时长）。**不是 429** —— 429 是调用方配额，这里是上游容量。
- **释放**：在 `finalize`/`closeout_attempt` 内按 `claim_id` 一次性跃迁，成功才 `concurrency_inflight -= 1`（`rpm_used` **不回退**，它是窗口计数）。
- **租约续期与回收**：与 canary claim 同规则 —— 随 attempt 心跳续租；后台只回收 `state='active' AND lease_expires_at < now()` 的。
- **未登记容量的 binding：整段跳过本事务**（第 31 轮修正——此前本条写"条件恒真、但仍插 claim 行"，与 [05 §4.3](./05-scheduling-and-operations.md) 的"未登记即不走 DB 原子路径"直接矛盾；而后者才是对的：**为了保住 P99≤50ms，未登记渠道不该在请求路径上多一次同步写**）。
  由应用层判 `concurrency_limit IS NULL AND rpm_limit IS NULL` 则**不执行本 SQL**，`capacity_ok` 直接填 1。
  代价：`/admin/health` 看不到这类渠道的实时并发 —— 这正是 [05 §4.3](./05-scheduling-and-operations.md) 要求界面显式提示"**未登记容量 → 保留未生效**"的原因。

**服务 FR/AC**：FR-029/032；AC-18。

---

### 6ter. 主动测活的请求模板与预算（FR-060~067，一期第二轨）

> ⚠️ **一条必须正面处理的冲突**（开发视角审查第 27 轮 [P0]）：设计要求测活用「**合格真实业务请求**」而非固定 `hello`/`ping`，但 [FR-112](../PRD.md) 明令**不存请求正文** —— 于是**根本没有正文可以复用**。canary 轨没问题（用的是内存里正在处理的真实请求），但主动测活要在**无业务流量时段**发请求，此刻手上一条正文都没有。
>
> **结论**：主动测活**只能用运维预先配置的探测模板**，不能、也无法复用用户历史正文。这不违反「不用 hello/ping」的本意——本意是"别用一个空洞的短请求去代表真实负载"，模板同样可以贴近真实工作负载（多轮上下文、工具定义、典型长度）。
>
> **代价（明示）**：模板探测的保真度**低于**真实业务请求——它测不出"这个渠道对我实际 prompt 分布的表现"。故**优先级永远是 canary 优先**（[05 §2](./05-scheduling-and-operations.md)），主动测活只在无流量时兜底。

```sql
-- 探测模板：运维维护，按模型 × 协议配置
CREATE TABLE probe_templates (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  model_id      BIGINT NOT NULL REFERENCES models(id),
  protocol      TEXT NOT NULL CHECK (protocol IN ('chat_completions','responses')),
  name          TEXT NOT NULL,                 -- 如 "typical-coding-turn"
  body          JSONB NOT NULL,                -- 探测请求体（**运维编写，非用户正文**，不受 FR-112 约束）
  expect_tools  BOOLEAN NOT NULL DEFAULT false,-- 是否期望触发 tool_calls（覆盖工具链路）
  expect_min_output_tokens INTEGER,            -- 低于此值视为异常响应
  enabled       BOOLEAN NOT NULL DEFAULT true,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (model_id, protocol, name)
);

-- 五维预算窗口（FR-063：全局/租户/用户/会话/资源，逐维独立计数）
CREATE TABLE probe_budget_windows (
  scope_kind    TEXT NOT NULL CHECK (scope_kind IN ('global','tenant','user','session','binding')),
  scope_id      TEXT NOT NULL DEFAULT '*',     -- 哨兵，避免可空列进主键
  window_start  TIMESTAMPTZ NOT NULL,          -- 日窗口，date_trunc('day')
  probe_count   INTEGER NOT NULL DEFAULT 0,
  probe_cost    nonneg_usd NOT NULL DEFAULT 0,
  failure_count INTEGER NOT NULL DEFAULT 0,    -- 供「测活失败 ≤ 错误预算 10%」判定
  PRIMARY KEY (scope_kind, scope_id, window_start)
);

-- 每次测活的占用（与 canary_claims 同构：原子 claim + 一次性释放，防并发突破）
CREATE TABLE probe_claims (
  claim_id      UUID PRIMARY KEY,
  binding_id    BIGINT NOT NULL REFERENCES bindings(id),
  template_id   BIGINT NOT NULL REFERENCES probe_templates(id),
  request_id    UUID NOT NULL,
  attempt_id    UUID NOT NULL,
  lease_owner   TEXT NOT NULL,
  lease_expires_at TIMESTAMPTZ NOT NULL,
  state         TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active','released')),
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  released_at   TIMESTAMPTZ
);
CREATE INDEX idx_probe_active ON probe_claims(binding_id) WHERE state = 'active';
```

**占用与释放**：与 canary 完全同构（[§6bis](#6bis-canary-占用fr-121跨实例硬上限的执行载体)）——五维窗口的条件 UPDATE + `INSERT probe_claims` + `INSERT attempts` **单事务**；释放在 `finalize`/`closeout_attempt` 内按 `claim_id` 一次性跃迁。**并发上限同样必须落库执行**，内存快照只做预筛。

**服务 FR/AC**：FR-060~067、FR-070~075；AC-09、AC-10。

---

### 6bis. canary 占用（FR-121，跨实例硬上限的执行载体）

> 硬上限不能只靠 `resource_health` 的计数列——那样双 core 各自放行即突破。占用必须有**可持久化的所有者**，且与 attempt 插入**同事务**（第 9 轮 [high]）。协议与原子 claim 语句见 [05 §2.0](./05-scheduling-and-operations.md)。

```sql
CREATE TABLE canary_claims (
  claim_id      UUID PRIMARY KEY,
  binding_id    BIGINT NOT NULL REFERENCES bindings(id),
  request_id    UUID NOT NULL,
  attempt_id    UUID NOT NULL,
  lease_owner   TEXT NOT NULL,                 -- 实例标识
  lease_expires_at TIMESTAMPTZ NOT NULL,       -- 初值 now()+60s；**须随 attempt 心跳续租**（见下）
  state         TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active','released')),
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  released_at   TIMESTAMPTZ
);
CREATE INDEX idx_canary_active ON canary_claims(binding_id) WHERE state = 'active';
```


**生命周期**：占用（条件 UPDATE + INSERT claim + INSERT attempt，**单事务**）→ **续租**（见下）→ 释放（`finalize` 内按 `claim_id` 一次性跃迁，跃迁成功才 `canary_inflight -= 1`）→ 回收（后台任务只收过期租约）。

**claim 租约必须与 attempt 租约同生命周期**（第 10 轮 [high]）：

> ⚠️ 上一版把租约写死 5 分钟且**没有任何续租动作**。一个合法的 canary 流只要跑超过 5 分钟，后台就会把**仍在执行**的 claim 回收并递减 `canary_inflight`，第二个请求随即拿到 claim——`canary_max_concurrent=1` 当场失效；原 attempt 结束时又因 claim 已 `released` 而无法修正计数。

- **续租**：`executor` 每次续写 `attempts.lease_heartbeat_at`（[§4.2bis](#42bis-悬挂-attempt-检测补齐-outbox-的盲区) 要求 ≤20s 一次）时，**在同一条语句/同一事务内**把关联 claim 的 `lease_expires_at` 一并延长到 `now() + 60s`。两个租约同源，不会出现"attempt 活着但 claim 过期"。
- **回收判据加严**：后台只回收 **`state='active'` 且 `lease_expires_at < now()` 且关联 attempt 已处于终态或同样失租**的 claim，回收时对 claim 行加锁并复核。仅凭 claim 过期不足以回收。
- **验收**：[AC-08](./14-acceptance-matrix.md) 须含**超过租约时长的长流用例**（如 5 分钟以上的 canary 流式请求），断言期间并发上限不被突破、claim 未被误回收。

**服务 FR/AC**：FR-121；AC-08⑥⑦⑧。

---

## 7. 采集快照 / 余额信号 / 凭证域

> 承接 [ISSUE-002 采集适配器](../issues/ISSUE-002-collector-adapter-design.md)。采集侧凭证明文（FR-113）；余额非实时、后台校对 + 信号自适应识别（FR-027、参数5）；快照标数据来源 + 更新时间 + 7 天有效（FR-011）；余额状态五态、订阅数据未知降级。

```sql
-- 采集侧凭证（FR-011/113；ISSUE-002 §4 凭证生命周期）：三家族不同续期机制，明文存储
CREATE TABLE collector_credentials (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  channel_id      BIGINT NOT NULL REFERENCES channels(id),
  site_family     TEXT NOT NULL CHECK (site_family IN ('newapi','sub2api','asxs','unknown')),
  cred_type       TEXT NOT NULL CHECK (cred_type IN
                    ('newapi_access_token','sub2api_jwt','asxs_jwt','account_password')),
  -- 明文（FR-113）；NewAPI 长期令牌 / Sub2API access+refresh / ASXS 7d JWT + 账号密码
  access_token    TEXT,
  refresh_token   TEXT,                         -- 仅 Sub2API（24h JWT + refresh 无密码续期）
  username        TEXT,                         -- ASXS 须账号密码重登 POST /api/manage/auth/login
  password        TEXT,                         -- 明文（一期）
  external_user_id TEXT,                        -- NewAPI New-API-User 头必需
  user_id_header_name TEXT,                     -- 二开 fan-out：New-API-User/Veloera-User/...（§3.1）
  token_expires_at TIMESTAMPTZ,                 -- Sub2API 24h / ASXS 168h；到期前阈值内续期
  -- 凭证互斥作废风险（ISSUE-002 §4）：NewAPI 重生令牌作废旧、Sub2API 并发刷新互斥
  refresh_lock_key TEXT,                        -- 按账号加互斥锁串行刷新
  status          TEXT NOT NULL DEFAULT 'valid'
                    CHECK (status IN ('valid','expiring','invalid','needs_relogin')),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 采集快照（FR-011/020/116；ISSUE-002 §5 降级一致性）：每次采集一行，标来源+时效
CREATE TABLE collector_snapshots (
  id              UUID PRIMARY KEY,             -- UUIDv7
  channel_id      BIGINT NOT NULL REFERENCES channels(id),
  scope_type      TEXT NOT NULL CHECK (scope_type IN ('account','key','group','subscription','pricing')),
  scope_id        TEXT,                         -- 对应资源标识
  payload         JSONB NOT NULL,               -- 归一后数值元数据（不存上游返回正文）
  data_source     TEXT NOT NULL,               -- auto_collect / manual（FR-011）
  fetched_at      TIMESTAMPTZ NOT NULL,
  valid_until     TIMESTAMPTZ                   -- 人工 7 天（FR-011）；过期按未知降级
  -- ⚠️ 不设 is_stale 存储列：PG 的 GENERATED ... STORED 要求表达式 immutable，
  -- 而 now() 非 immutable（且存量值也不会随时间自动变旧）。陈旧性一律**查询期计算**：
);

-- 陈旧性视图（替代原先非法的 is_stale 生成列）
CREATE VIEW collector_snapshots_v AS
  SELECT *, (valid_until IS NOT NULL AND valid_until < now()) AS is_stale
  FROM collector_snapshots;

-- 余额信号状态（FR-020~027；参数5 信号自适应识别）：非实时，后台校对 + 多判据
CREATE TABLE balance_signals (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  account_id      BIGINT REFERENCES upstream_accounts(id),
  key_id          BIGINT REFERENCES upstream_keys(id),
  -- 余额五态（PRD §9.2）
  balance_state   TEXT NOT NULL DEFAULT 'unknown'
                    CHECK (balance_state IN ('normal','critical','unknown','exhausted','abnormal')),
  -- 保守下限（FR-026）：最近可信余额 - 已知消耗
  last_confirmed_balance nonneg_usd,            -- 归一美元
  confirmed_at    TIMESTAMPTZ,
  known_consumption_since nonneg_usd,           -- 自确认点后的已知消耗（形成保守下限）
  conservative_floor usd_amount,                -- = last_confirmed - known_consumption；**可为负**，故用可正负的 usd_amount
  -- 信号自适应识别（FR-027、参数5）：组合错误码/文案正则/真实失败信号/余量归零
  signal_kind     TEXT CHECK (signal_kind IN ('error_code','error_text_regex','real_request_fail','quota_zeroed')),
  signal_evidence TEXT,                         -- 触发信号原文关键词（元数据，非正文）
  -- 配额状态（FR-118、ISSUE-001 假设5）：**未知默认保守排除**，由我方 selector 执行
  quota_status    TEXT CHECK (quota_status IN ('available','warning','exhausted','unknown')),
  -- ↑ 由采集器按站型映射（NewAPI/Sub2API/ASXS 各自的余量与状态字段）；`unknown` → selector 默认排除（FR-118）。
  --   ⚠️ 该保守默认是我方硬要求：任何上游或第三方组件的宽松默认（"未知即保留"）不得覆盖它。
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

#### 7.1 `collector_snapshots.payload` 的逐 scope_type 结构（**P1 必需**，第 45 轮补）

> ⚠️ **此前只写了"归一后数值元数据"** —— 而 P1 的 [`GET /admin/keys/{id}/usage`](./09-admin-api.md) 被定义为"读该 payload 的时序"，[04 §4](./04-collector-adapter.md) 也要求把 Key 用量历史与分组的高峰倍率等字段写进 payload。**写入侧与读取侧都没有可实现的契约**（开发视角审查第 45 轮）。
>
> **总则**：payload 只存**归一后的数值与枚举**（金额一律 `usd_amount` 数值、时间一律 ISO8601 字符串），**不存上游返回正文**（FR-112）。键名一律 snake_case。**未采到的字段一律省略该键**，不写 `null` —— 便于区分"没采到"与"采到的值是 0"。

| `scope_type` | `scope_id` 填什么 | payload 必备键 | 可选键 |
| --- | --- | --- | --- |
| `key` | `upstream_keys.id`（十进制字符串） | `remain_quota_usd`、`used_quota_usd` | `request_count`（上游累计请求数，NewAPI 有）、`window_usage`（Sub2API 的 5h/1d/7d 窗口用量，形如 `{"5h":{"limit_usd":x,"usage_usd":y,"window_start":"…"},"1d":{…}}`）、`current_concurrency`、`expired_time`、`rpm_limit`、`concurrency_limit` |
| `group` | `channel_groups.group_ref` | `rate_multiplier` | `peak_enabled`、`peak_start`、`peak_end`、`peak_rate_multiplier`、`is_exclusive`、`platform`、`subscription_type`、`rpm_limit` —— **这几项 P1 只进 payload、不落结构化列**（[§1.3](#13-上游分组与模型目录交付阶段-p1fr-123127) 的取舍表），P2/P3 需要时按本表回填 |
| `account` | `upstream_accounts.id` | `balance_usd` | `used_usd`、`external_user_id`、`quota_per_unit`（NewAPI 的额度换算基数，逐站不同、**不可写死**） |
| `pricing` | `models.canonical_name` 或上游原始模型名 | `input_price`、`output_price` | `cache_price`、`billing_unit`、`group_ratio`、`completion_ratio` |
| `subscription` | ⏭ P4 | — | 订阅制整体推迟，P1~P3 不写该 scope |

- **`GET /admin/keys/{id}/usage` 的读取契约**：按 `scope_type='key' AND scope_id=<id>` 取，按 `fetched_at` 升序返回 `{fetched_at, remain_quota_usd, used_quota_usd, request_count?}` 序列。缺键的点位**跳过该字段**而不是填 0（填 0 会在曲线上造出假的"额度归零"）。
- **为何 `scope_id` 用 TEXT 存数字 id**：该列是跨 scope 复用的通用标识（`group` 用的是字符串 `group_ref`），故统一 TEXT；读取侧自行转换。

**索引**

```sql
CREATE INDEX idx_cred_channel     ON collector_credentials(channel_id, status);
-- 一渠道一份采集凭证（第 46 轮补）：续期是 upsert 语义（refresh 后写回新令牌，
-- 04 §5），没有唯一约束就只能"先查再插/改" —— 那是 check-then-act，
-- 而凭证刷新恰恰并发敏感（不变式 S-1）。多账号分别采集属后续阶段。
CREATE UNIQUE INDEX idx_cred_channel_unique ON collector_credentials (channel_id);
CREATE INDEX idx_snap_scope       ON collector_snapshots(channel_id, scope_type, fetched_at DESC);
CREATE INDEX idx_snap_stale       ON collector_snapshots(valid_until) WHERE valid_until IS NOT NULL;
CREATE INDEX idx_balsig_account   ON balance_signals(account_id);
CREATE INDEX idx_balsig_state     ON balance_signals(balance_state) WHERE balance_state<>'normal';
CREATE INDEX idx_balsig_quota     ON balance_signals(quota_status) WHERE quota_status='unknown'; -- FR-118 保守排除
```

**服务 FR/AC**：FR-010/011、FR-020~027、FR-031、FR-113、FR-116、FR-118；AC-28（三家族探测）、AC-29（非标准余额不足识别）。

---

## 8. 告警与事件域（合并持续事件）

> FR-100~103：告警含严重等级/影响范围/触发数据/建议动作；同因合并为一个持续事件；余额耗尽/Key 失效/全资源不可用不得延迟。

```sql
-- 持续事件（FR-102）：同一原因的重复异常合并为一个事件
CREATE TABLE alert_events (
  id              UUID PRIMARY KEY,             -- UUIDv7
  dedup_key       TEXT NOT NULL,               -- 同因合并键（FR-102）
  severity        TEXT NOT NULL CHECK (severity IN ('P1','P2','P3')), -- 参数16：P1 15min/P2 1h/P3 当日
  category        TEXT NOT NULL,               -- 取值与 dedup_key 规则见 [05 §5.2bis](./05-scheduling-and-operations.md)：
                                              -- balance_exhausted/key_invalid/all_unavailable/billing_anomaly/
                                              -- collector_failed/unknown_billing/error_budget_burn/
                                              -- probe_template_missing/capacity_tight/data_stale
  occurrence_count INTEGER NOT NULL DEFAULT 1, -- 同因重复发生次数（FR-102 合并而非刷屏）
  payload         JSONB,                      -- 触发时的判据快照（元数据，**不含正文**，FR-112）
  scope           JSONB,                       -- 影响范围（渠道/资源/故障域）
  trigger_data    JSONB,                       -- 触发数据（元数据）
  suggested_action TEXT,                        -- 建议处置（FR-101）
  state           TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open','acknowledged','recovering','closed')),
  started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_seen_at    TIMESTAMPTZ NOT NULL DEFAULT now(), -- 同因重复发生时刷新（[05 §5.2bis](./05-scheduling-and-operations.md) 合并事务）
  acknowledged_at TIMESTAMPTZ,
  -- 进入 recovering 的时刻（第 41 轮补）：关闭判据是「recovering 持续超 alert_close_after_sec」，
  -- 没有这一列就只能拿 started_at 或 last_seen_at 凑 —— 前者会让开了几小时的告警一恢复就立刻关，
  -- 后者会让它永远关不掉。转出 recovering（判据再次成立）时必须置回 NULL。
  recovering_since TIMESTAMPTZ,
  closed_at       TIMESTAMPTZ,
  -- ⚠️ 不可用 UNIQUE(dedup_key, state)（对抗性审查发现）：state 不同即视为不同行，
  --    同因可同时存在 open/acknowledged/recovering 三行；且历史上只允许一条 closed，
  --    导致同键再次发生后无法转为 closed —— 与"同因只有一个持续事件"完全相反。
  CONSTRAINT alert_state_valid CHECK (state IN ('open','acknowledged','recovering','closed'))
);

-- ✅ 正确做法：**部分唯一索引** —— 同 dedup_key 在"未关闭"状态下只允许一行；
--    closed 行不受限，可累积任意多条历史（AC-19 的去重与生命周期恢复）
CREATE UNIQUE INDEX uq_alert_active ON alert_events(dedup_key) WHERE state <> 'closed';

CREATE INDEX idx_alert_open ON alert_events(severity, started_at) WHERE state<>'closed';

```

> 生命周期转换（open→acknowledged→recovering→closed）须在**行锁**下进行（`SELECT … FOR UPDATE`），
> 避免并发转换产生第二条活动行。如需保留"同因重复发生"的明细，另建 `alert_occurrences` 子表，
> 不在主表堆积。

**服务 FR/AC**：FR-100~103、FR-105；AC-19（余额耗尽/Key 失效即时告警到关闭）。

---

## 9. 保留与清理策略

> FR-112：账本元数据保留 ≥180 天且可查询；一期不存正文/上下文/头/体。多实例共享（FR-110）。

### 9.1 分区与保留

| 表 | 分区键 | 分区粒度 | 保留 | 清理方式 |
| --- | --- | --- | --- | --- |
| `requests` | `created_at` | 月 RANGE | ≥180 天（默认保留 7 个月分区） | 到期分区 `DETACH` → `DROP TABLE`（秒级，不锁热表） |
| `attempts` | `request_created_at` | 月 RANGE | 同上，与 requests 对齐 | 同上；对齐分区键保证联表在同月 |
| `attempt_usage` | `request_created_at` | 月 RANGE | 同上 | 同上 |
| `quality_events` | `detected_at` | 月 RANGE（可选） | ≥180 天 | 同上 |
| `session_prefix_ledger` | `created_at` | 无分区（量小） | ≥180 天 | 按 `created_at` 批量 `DELETE`（低频） |
| `health_metric_windows` | `window_start` | 无分区 | 滚动 30 天（仅供健康重算，非账本） | 后台按 `window_start < now()-30d` 清理 |
| `collector_snapshots` | `fetched_at` | 无分区 | 保留最近 N 次 + 90 天（快照可重建） | 保留每 scope 最新若干版 + 时间清理 |

```sql
-- 分区管理：提前建下月分区 + 滑动窗口 DROP（由后台定时任务或 pg_partman 执行）
-- 例：建 2026-08 分区
CREATE TABLE requests_2026_08 PARTITION OF requests
  FOR VALUES FROM ('2026-08-01') TO ('2026-09-01');
CREATE TABLE attempts_2026_08 PARTITION OF attempts
  FOR VALUES FROM ('2026-08-01') TO ('2026-09-01');
CREATE TABLE attempt_usage_2026_08 PARTITION OF attempt_usage
  FOR VALUES FROM ('2026-08-01') TO ('2026-09-01');

-- 保留策略：DETACH 后 DROP 超过 180 天（7 分区）的最老月
-- ALTER TABLE requests DETACH PARTITION requests_2026_01; DROP TABLE requests_2026_01;
```

### 9.1bis DDL 可执行性门禁（**必须做，已三次栽在这里**）

> 本文档的 DDL 曾出现三类**光看不出、一跑就炸**的错误：`attempt_usage` 双主键、`is_stale` 用 `now()` 做 stored generated column、`channels` 外键前向引用未创建的 `upstream_providers`。**人眼评审挡不住这类问题。**

**门禁规则（M0 起生效，纳入 CI）**：

1. `migrations/` 的完整 DDL **必须在一次性 PostgreSQL 实例上真实执行成功**，才算 schema 基线通过。
2. CI 每次跑：起临时 PG → 按序执行全部迁移 → 建分区 → 执行一遍 `sqlc generate` → 全部成功才绿。
3. 本文档的 DDL 与 `migrations/` **以后者为准**；文档变更若涉及 DDL，须同步迁移文件并通过门禁。
4. 附加断言（[9.2](#92-保留策略配置化) 的 FR-112 守卫同批执行）：账本表禁止出现 `body/messages/prompt/headers` 命名列。

> 建表顺序原则：**被引用的表先建**。当前依赖链为
> `upstream_providers → channels → upstream_accounts → upstream_keys → models → cache_scopes → bindings`、
> `routing_policies → model_aliases → sla_targets`、
> `requests → attempts → attempt_usage / ledger_outbox`。

**门禁必须覆盖的三类"抽出来就跑不了"**（第 18 轮实测发现两处）：

| 类别 | 检查 | 曾踩过的实例 |
| --- | --- | --- |
| **前向外键** | 任一 `REFERENCES X` 出现时 `X` 必须已 `CREATE TABLE` | `model_aliases.policy_id REFERENCES routing_policies` 早于 `routing_policies` 定义 |
| **索引早于表** | `CREATE INDEX ... ON X` 必须晚于 `CREATE TABLE X` | `idx_outbox_undelivered ON ledger_outbox` 写在 §4.2bis，而 `ledger_outbox` 定义在 §9.2bis |
| **代码块纯净** | ` ```sql ` 块内不得出现 Markdown（`>` / `|` / `**` / `#`） | 第 16 轮在 `alert_events` 后混入三行引用 |

> **已实测（2026-07-26）**：`verify/ddl-check.sh` 从本文件抽出全部 **88 条 DDL**，在 `postgres:16` 上以 `ON_ERROR_STOP=1` 执行**通过**，建出 45 个 public 对象（44 表 + 1 视图，另含 2 域 + 41 索引）。
> 脚本同时做一条**回归断言**——第 18 轮那个 critical 的实际行为：`auth_rejections` 的**匿名 401**（`gateway_client_id` 与 `secret_prefix` 全 NULL）可写入，且同分钟同组合经 `ON CONFLICT` 只增计数不新增行（1 行 / 计数 2），`UNIQUE NULLS NOT DISTINCT` 语义符合预期。**这是实测，不再是推断。**
>
> ⚠️ **该脚本的边界**：只验证「建表能否跑通」。**不验证**分区子表创建、分区表上的外键行为、以及事务骨架里那些带 `:参数` 的伪 SQL —— 后者必须在 M0 用真实查询覆盖，不能因为本脚本绿了就认为账本逻辑已验证。
>
> 这三类都**不是语义争议，是机器可判定的**：CI 从文档抽取全部 ```sql 块按出现顺序拼成迁移、在临时 PG 上真跑一遍即可全部暴露。**本节的价值就在于它不依赖人读**——前 17 轮的人工审查都没发现这两处顺序问题。

### 9.2 保留策略配置化

- 保留窗口（默认 7 个月 ≥180 天）走 `config_params(param_key='retention_months', is_critical=true)`；缩短保留是关键操作，需二次确认（FR-115）。
- **不可存列的守卫**：CI 中加 schema 断言，禁止任何账本表出现 `body/messages/prompt/completion_text/headers` 命名列（FR-112 硬约束 3）。

### 9.2bis 账本 outbox（防崩溃丢账，[01 §5.1](./01-architecture.md)）

账本状态更新不得只存在于内存队列——进程崩溃即永久丢账（账本是唯一真相源，无处可对账）。异步更新一律先写 outbox（与业务行同事务），再由后台投递：

```sql
CREATE TABLE ledger_outbox (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  attempt_id    UUID NOT NULL,
  request_created_at TIMESTAMPTZ NOT NULL,     -- 用于定位分区
  event_type    TEXT NOT NULL CHECK (event_type IN
                  -- ⚠️ outbox **只承载异步事实**（第 15 轮 [critical]）。
                  --    首字与终帧因为要「先落库再放行字节」，已改为**同步直写表**，
                  --    不再经 outbox —— 故 'first_token'/'attempt_end'/'usage'/'request_close'
                  --    四个旧类型已删除，避免出现「同一事实两条写入路径」。
                  ('downstream_first_byte',        -- 首字节写出 → 写 attempts.downstream_first_byte_written_at
                   'downstream_write_completed',   -- 终帧写出   → 触发 finalize_delivery
                   'cancel')),                     -- 每跳取消原因/传播标记
  payload       JSONB NOT NULL,                -- 状态更新的元数据（不含正文，FR-112）
  -- 幂等键：同一 attempt 的同一状态跃迁只生效一次，重放不产生重复/不覆盖更晚状态
  idempotency_key TEXT NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  delivered_at  TIMESTAMPTZ,                   -- NULL = 待投递
  UNIQUE (idempotency_key)
);

CREATE INDEX idx_outbox_pending ON ledger_outbox(created_at) WHERE delivered_at IS NULL;
-- 支撑 §4.2bis 恢复扫描的 NOT EXISTS 反连接（必须建在本表之后）
CREATE INDEX idx_outbox_undelivered ON ledger_outbox(attempt_id, request_created_at, event_type)
  WHERE delivered_at IS NULL;
```

**写入点**（第 42 轮补：整张表此前**只有建表、索引与读取**——恢复扫描 `SELECT 1 FROM ledger_outbox`、
投递器 drain、`finalize_delivery` 由它驱动，**却没有任何一条 INSERT**。
防崩溃丢账的整套机制没有入口，开发照文档实现会得到一张永远空的表，
于是 `finalize_delivery` 永不触发、所有请求停在 `pending`）：

| 事件 | 谁写 | 何时 | `payload` |
| --- | --- | --- | --- |
| `downstream_first_byte` | `executor` | 向下游 socket 写出**首字节**的 `Write()` 返回后 | `{"attempt_id":…,"written_at":…}` |
| `downstream_write_completed` | `executor` | 写出**终帧**的 `Write()` 返回后 | 同上 |
| `cancel` | `executor` | 每跳取消时记原因与传播结果 | `{"attempt_id":…,"cancel_reason":…,"propagated":bool}` |

```sql
-- 三类事件共用同一条语句（event_type / payload 不同）。**不在业务事务里**：
-- 这三件事都发生在「字节已经写出去之后」，此时业务事务早已提交。
INSERT INTO ledger_outbox(attempt_id, request_created_at, event_type, payload, idempotency_key)
VALUES (:attempt_id, :rcat, :event_type, :payload, :attempt_id || ':' || :event_type)
ON CONFLICT (idempotency_key) DO NOTHING;      -- 重放/重试只投一次
```

> ⚠️ **`idempotency_key` 必须是 `<attempt_id>:<event_type>`**（[§2ter](#2ter-幂等键一览) 已登记）。
> 用随机 UUID 会让每次重试都被当成新事件 —— 首字节时间戳被反复覆盖、`finalize_delivery` 反复触发。

**投递（drain）**：后台 goroutine 每秒一轮，**取出 → 执行对应事务 → 标记已投递**。

```sql
-- ⚠️ 标记与执行必须同事务，否则崩在中间会重复投递（幂等键只挡重复插入，不挡重复执行）
WITH claimed AS (
  SELECT id, attempt_id, request_created_at, event_type, payload
    FROM ledger_outbox
   WHERE delivered_at IS NULL
   ORDER BY created_at
   FOR UPDATE SKIP LOCKED                       -- 多实例安全
   LIMIT :batch)
UPDATE ledger_outbox o SET delivered_at = now()
  FROM claimed WHERE o.id = claimed.id
RETURNING o.attempt_id, o.request_created_at, o.event_type, o.payload;
-- 应用层按 event_type 分派：downstream_write_completed → finalize_delivery（§2bis）
```

- **投递失败不清 `delivered_at`**：本轮事务整体回滚，行仍是 `NULL`，下一轮自然重取。
- **`delivered_at` 的唯一写入者是本 drain**，不要在别处标记。

**恢复流程**：实例启动时先 drain 一遍 `delivered_at IS NULL` 的行（同上语句），再进 §4.2bis 的悬挂扫描——
顺序不可颠倒（[§4.2bis](#42bis-崩溃恢复扫描) 已定：已写完的请求靠 drain 收口，根本不该进恢复扫描）。
因幂等键存在，重放不会产生重复账目。

### 9.3 多实例并发写（FR-110）

- 账本主键 UUIDv7 由各 sla-core 实例本地生成，无序列争用；同一 `request_id` 下的多 attempt 由持有该请求的实例串行写，无跨实例竞争。
- 账本写入由持有该请求的实例完成，无跨实例竞争；**转向自研后无对账器**（[11](./11-decision-full-selfbuilt.md)），usage 直接来自旁路观察的终帧（[03 §7](./03-upstream-layer.md)）。
- **崩溃恢复**：任一实例启动时重放 `ledger_outbox` 的未投递行（§9.2bis），包括**其他已崩溃实例**遗留的——outbox 在库中，不随进程消失。
- 后台快照（价格/健康/余额）写路径与同步决策路径解耦：决策只读内存快照，PG 抖动不阻塞（01-架构 §5）。

---

## 10. FR / AC 覆盖矩阵（逐表回溯）

| 域 / 表 | 主要 FR | 主要 AC |
| --- | --- | --- |
| §1 注册（channels/accounts/keys/models/bindings/fault_domains/cache_scopes） | FR-001~006、022、031、044/045、055、095、109、113 | AC-01/04/05/11 |
| §2 别名与策略（model_aliases/routing_policies/sla_targets/config_params） | FR-062、090/091、104、115/116/117 | AC-25/26 |
| §2bis 入站凭证与配额（gateway_clients/client_daily_spend/client_rate_window/client_reservations/reservation_adjustments/**auth_rejections**） | FR-094、113、120 | AC-33 |
| §2ter 数据许可（data_policies） | FR-093（P1，默认关） | AC-14 |
| §3 价格版本（price_versions/price_change_log） | FR-010、012~018 | AC-02/03/17 |
| §4 请求与 Attempt 账本（requests/attempts/attempt_usage/session_prefix_ledger） | FR-040、050/051、058、070~072、076/078/079/080、092、097~099、112、116、**119** | AC-06/07/09/12/13/16、**AC-30/31/32** |
| §5 订阅台账（subscription_plans/user_subscriptions/quota_windows/waste_forecast） | FR-011、033~039、057/058 | AC-20/21/22/23/**24** |
| §6 健康/冷却/样本（resource_health/health_metric_windows/quality_events；canary 占用表 `canary_claims` 见 [05 §2.0](./05-scheduling-and-operations.md)） | FR-007、040~047、060/065/066、**121**（canary 受控验证） | AC-08/10 |
| §7 采集/余额/凭证（collector_credentials/collector_snapshots/balance_signals） | FR-010/011、020~027、031、113、116、**118** | AC-28/29 |
| §8 告警（alert_events） | FR-100~103、105 | AC-19 |

### 10.1 与硬约束的对齐（转向自研后）

> 下列约束原由 [ISSUE-001](../issues/ISSUE-001-tech-assumption-verification.md) 的运行时验证提出（针对外部网关）；转向自研后（[11](./11-decision-full-selfbuilt.md)）**约束本身依然成立**，只是落点从"网关字段 + 对账"变为"自研旁路观察"。

| 硬约束 | 数据层落点 |
| --- | --- |
| 自算内容感知 TTFT，不采信任何上游首字信号 | `attempts.content_aware_ttft_ms`（旁路观察判定，[03 §3.2](./03-upstream-layer.md)）；`gateway_reported_ttft_ms` 列保留供上游若回传该类字段时**仅存证** |
| 取消/失败口径统一（客户端断开 / 上游断流 / SLA 取消 / 上游错误） | `attempts.cancel_reason`（归并枚举）+ `error_message`（原文存证） |
| 配额未知默认保守排除（FR-118） | `balance_signals.quota_status`，`unknown` → selector 排除（`idx_balsig_quota`） |
| **一次外部调用 = 一次上游调用**（FR-119） | 自研层**不做隐藏重试**，`upstream_call_count` 恒为 1；`hidden_retry_detected` 仅在接入会隐藏重试的第三方通道时才可能为真（一期不存在） |
| 账本自洽（usage/cost 由旁路观察终帧提取，价格版本自持） | `attempts.upstream_response_id`、`attempt_usage.*` → `price_versions`（[03 §7](./03-upstream-layer.md)） |
| 每跳显式落账，无外部账本需比对 | `attempts.attempt_no/role` 表达 `executor` 驱动的每一跳；**单一真相源** |

### 10.2 与 ISSUE-002 真实字段的对齐

| 采集真实字段（ISSUE-002） | 数据层落点 |
| --- | --- |
| sub2api `user_subscriptions.daily/weekly/monthly_window_start + usage_usd` | `subscription_quota_windows`（三窗口拆行） |
| sub2api 共享额度按 `(user_id, group_id)` 聚合 | `user_subscriptions(ext_user_id, group_id)` UNIQUE + `idx_subs_shared` |
| sub2api 无超额概念 | `subscription_plans.overage_rule='no_overage_block'` |
| ASXS `primarySource/secondarySource` | `subscription_plans.primary_source/secondary_source` |
| ASXS `dailyReset`（usageThresholdPercent/dailyLimit） | `subscription_plans.active_reset_*`（只读登记，不自动触发） |
| ASXS `priceCnyCent + durationDays` / sub2api `price + validity_days` | `subscription_plans.fixed_fee + validity_days` → 双倍率输入 |
| 三家族凭证/续期差异（长期令牌/refresh/账号密码重登） | `collector_credentials.cred_type + refresh_token/username/password` |
| 三家族额度单位（quota整数 / USD / micros）归一 | 入库前由适配器归一为 `usd_amount`（§0.2） |

---

## 11. 开放点（评审需拍板）

| # | 开放点 | 建议 |
| --- | --- | --- |
| 1 | 分区自动化用 `pg_partman` 还是自研定时任务 | ✅ **已定**：一期自研定时任务（单机、依赖少）；量级上来再评估 pg_partman |
| 2 | `session_id` 来源（不存正文如何标识会话） | ✅ **已收口**（见 §4.5）：**多源提取、不要求调用方配合**——主流客户端本就自带（Claude Code `X-Claude-Code-Session-Id`、Codex `session_id`/`conversation_id`、Responses `conversation`/`prompt_cache_key`）；按优先级捞，全未命中则按单轮处理、不派生不编造 |
| 3 | ~~AxonHub 换 PG 共库 vs 独立 SQLite~~ | ⛔ **已废止（2026-07-25，[11 转向决策](./11-decision-full-selfbuilt.md)）**：数据面已无 AxonHub，**不存在共库分 schema，也不存在对账 JOIN**。当前结论：**单库单 schema，账本为唯一真相源**。原实测记录见 [07 §1](./07-axonhub-runtime-probes.md)，仅作历史，**不得作为实施指令** |
| 4 | `decision_snapshot` JSONB 体积（每请求一份候选/排除快照） | ✅ **已定**：元数据裁剪 + 仅存 binding_id 与原因码，不存完整对象；必要时挪冷分区压缩 |
| 5 | 隐藏重试补算的触发点 | ⛔ **已废止（2026-07-25）**：原方案依赖外部网关审计比对，转向后**无网关、无对账阶段**。当前结论：**一期不做隐藏重试推断**——我们自己不发隐藏重试（FR-119），`attempt_usage.upstream_seq` 恒为 1；上游中转站内部若有隐藏重试，我们**不可观测也不补算**，其成本已体现在上游回传的 usage 里。**因此 `attempts.upstream_call_count` 恒为 1、`hidden_retry_detected` 恒 false、`hidden_retry_kind` 恒 NULL**——三列保留建表只为二期零改表，**一期无写入方是设计如此，不是漏实现**（第 40 轮明示） |
| 6 | 一期入站协议范围（FR-111 现为 CC + Responses） | ✅ **已定（2026-07-23）：维持 CC + Responses，不扩** —— **主力客户端为 Codex CLI，说的正是 OpenAI Responses，已在一期范围内**。Claude Code（Anthropic Messages）与 Gemini CLI（Gemini API）一期不直连；提取规则保留在 §4.5 仅为将来扩协议时零改动 |

---

_本篇为 M0 前的数据模型基线。任何字段变更须回溯到具体 FR/AC 或 ISSUE-001/002 的运行时事实，不得凭空增列；账本表新增列前须过 FR-112「不存正文」CI 断言。_
