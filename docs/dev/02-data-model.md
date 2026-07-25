# 02 数据模型（自研 SLA 核心 · PostgreSQL 单库 · v0.1 草案）

| 项目 | 内容 |
| --- | --- |
| 状态 | 草案，待评审（M0 前定稿） |
| 日期 | 2026-07-23 |
| 栈 | Go（pgx + sqlc）/ PostgreSQL 单库（一期不引 Redis）/ 单机 Docker Compose / **无外部网关**（[11 转向](./11-decision-full-selfbuilt.md)） |
| 输入 | [PRD v1.3](../PRD.md)（FR-001~119、AC-01~32、§11 默认策略、§13 参数）、[DECISIONS](../DECISIONS.md)（16 参数 + 5 新需求）、[ISSUE-001 运行时结论](../issues/ISSUE-001-tech-assumption-verification.md)（六假设 + beta5 schema 适配表）、[ISSUE-002 采集适配器](../issues/ISSUE-002-collector-adapter-design.md)（三家族、sub2api ent schema、凭证生命周期）、[ISSUE-002 探测实测](../issues/ISSUE-002-probe-results.md)（四站真实字段）、[00 总览](./00-overview-and-milestones.md)（硬约束 11 条）、[01 架构](./01-architecture.md)（sla-core 模块、自研上游直连） |
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
- 枚举一律用 `TEXT + CHECK` 约束（便于 beta5 升级时扩枚举，不用 ALTER TYPE）。
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
  max_context   INTEGER,
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
  effective_url TEXT NOT NULL,    -- 实际上游 URL（attempt 的 binding 三要素之一：渠道+key+url）
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (channel_id, account_id, key_id, model_id, region, cache_scope_id)
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

## 2. 别名与策略域（对外别名即策略载体）

> FR-062/117：调用方通过选择**对外模型别名**选择策略（SLA 等级、是否允许测活）。业务不逐请求标注敏感属性。策略需版本化可回滚（FR-104）、全部可配置 + 关键项二次确认（FR-115）。

```sql
-- ⚠️ routing_policies 必须先于 model_aliases 创建（后者有外键指向它），
-- 定义见下方"路由策略"；此处仅提示建表顺序，实际 DDL 在 migrations/ 中按依赖排序。

-- 对外模型别名（FR-062/117、AC-25）：如 gpt-5.5（可测活） vs gpt-5.5-sla-1（禁测活）
CREATE TABLE model_aliases (
  id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  alias          TEXT NOT NULL UNIQUE,        -- 对外暴露名
  target_model_id BIGINT REFERENCES models(id),
  policy_id      BIGINT NOT NULL REFERENCES routing_policies(id),
  enabled        BOOLEAN NOT NULL DEFAULT true,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 路由策略（FR-090/104/115）：一套具体策略，别名映射到它
CREATE TABLE routing_policies (
  id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name           TEXT NOT NULL,               -- 如 sla-gold / sla-silver / sla-bronze（仅默认模板，可自定义）
  sla_level      TEXT NOT NULL,               -- 等级名可配置（DECISIONS 参数1：等级数量/数值全自定义）
  probe_allowed  BOOLEAN NOT NULL DEFAULT false, -- 该别名是否允许测活（FR-062，*-sla-* 默认禁测活）
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
  rpm_limit     INTEGER,                       -- 每分钟请求数上限
  -- 生命周期
  status        TEXT NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active','revoked','expired')),
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
单跳上界(binding) = ( 输入 token 上界 × 该 binding 输入单价
                    + models.max_output_tokens × 该 binding 输出单价 ) × 倍率版本

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

**实际费用超出预留时**（上游不遵守上限等）：结算按**实际值**写 `settled_usd`；若结算后 `settled_usd + reserved_usd > quota_daily_usd`，立即置该 client 当日 `over_quota` → **后续请求一律 429**。**已完成的请求不追溯拒绝**（无法收回）。此为可接受的有界溢出，AC-33 须断言"溢出后下一请求必被拒"。

**统一终结事务 `finalize(request_id, outcome)`**（**唯一被允许修改聚合计数器的代码路径**）：

> 对抗性审查第 7 轮 [critical]：预留在请求发起**前**创建，而"上游未发出即崩溃"的恢复只把 attempt 置 `failed`，**没有释放预留** → 每次这类崩溃永久占用一份日配额，累积到最后所有合法请求都收 429。人工把聚合值改回去又重新引入重复扣减。故三条路径必须共用同一个幂等事务。

```sql
BEGIN;
-- ① 唯一闸门：per-request 行的状态跃迁。重放/并发时行数=0 → 直接 COMMIT，聚合不动
--    ⚠️ RETURNING 出 client/日期/预留额 —— **聚合表只能用这三个返回值**，
--       不接受调用方传参（第 11 轮 [high]：跨午夜长流、恢复事件携带旧字段、
--       实现参数错配都会从**错误日期或错误客户**扣预留，且 reservation 已终态、
--       幂等重放再也修不回来 → 永久配额泄漏或跨租户账目污染）。
WITH settled AS (
  UPDATE client_reservations
     SET state            = :new_state,      -- 'settled' | 'abandoned'
         actual_usd       = :actual_usd,     -- abandoned 时为 NULL；非负（domain 约束）
         settle_event_key = :event_key,
         needs_manual_review = :needs_review,
         settled_at       = now()
   WHERE request_id = :rid AND state = 'reserved'
  RETURNING gateway_client_id, spend_date, estimated_usd, actual_usd
)
-- ② settled 为空（重放/并发）→ 本语句影响 0 行，聚合不动
UPDATE client_daily_spend d
   SET reserved_usd = d.reserved_usd - s.estimated_usd,
       settled_usd  = d.settled_usd  + COALESCE(s.actual_usd, 0)
  FROM settled s
 WHERE d.gateway_client_id = s.gateway_client_id
   AND d.spend_date        = s.spend_date;
-- ③ 同事务推 **attempt** 终态（由上游 terminal_event 决定，与下游是否写完无关）
UPDATE attempts SET attempt_status = :attempt_terminal, ended_at = now()
 WHERE request_id = :rid AND attempt_status IN ('pending','committed');
-- ⚠️ **request 的终态不在这里定**（第 12 轮 [critical]）：见下方两阶段说明
COMMIT;
```

> **`finalize` 的入参只有** `request_id`、`new_state`、`actual_usd`、`event_key`、`needs_review`、两个终态。**`gateway_client_id`/`spend_date`/`estimated_usd` 一律由 `RETURNING` 派生**——与 `adjust` 同一条纪律。跨午夜的长请求因此天然把费用记在**预留那一天**。

**关单必须两阶段：`finalize_upstream` → `finalize_delivery`**（第 12 轮 [critical]）：

> ⚠️ 上一版让 `finalize` 在**终帧到达时**就把 `requests.final_status` 写成 `completed`。但那一刻终帧**还没写给下游**（[03 §3.0](./03-upstream-layer.md) 要求先落库再放行）、`downstream_write_completed_at` 也还不存在。于是随后落入 K4 窗口崩溃时，request **早已是终态**，恢复 SQL 的 `WHERE final_status='pending'` 闸门再也改不动它 —— **第 11 轮新增的 K4 语义被正常关单路径整个绕过**，等于白加。

| 阶段 | 触发 | 写什么 | 同步性 |
| --- | --- | --- | --- |
| **`finalize_upstream`** | 上游**终帧到达**（成本此刻已确定） | reservation 结算（上方 SQL）+ `attempt_status` 终态 + `terminal_event`/usage | **同步**，且必须在放行终帧字节**之前**提交 |
| **`finalize_delivery`** | 下游 **socket write 返回之后** | `downstream_write_completed_at` + `requests.final_status` | 异步经 outbox（不涉及计费） |

```sql
-- finalize_delivery：request 终态由「我们是否写完」决定
UPDATE requests r
   SET final_status = CASE
         WHEN a.downstream_write_completed_at IS NOT NULL
              AND a.attempt_status = 'completed'          THEN 'completed'
         WHEN a.attempt_status IN ('failed','unknown_billing') THEN 'failed'
         ELSE 'interrupted'                                -- 我们没写完 → 保守
       END
  FROM attempts a
 WHERE r.id = :rid AND a.request_id = :rid AND a.attempt_no = :last_attempt_no
   AND r.final_status = 'pending';                         -- 幂等：只推一次
```

- **计费与交付彻底解耦**：钱在 `finalize_upstream` 就结清（成本那时已知），交付结论晚一步不影响配额正确性。
- **崩溃在两阶段之间** = 正是 K4：reservation 已 settled、attempt 已终态、request 仍 `pending` → 恢复扫描按 §4.2bis ③c 判 `interrupted`。**这条路径现在真的可达了。**

**两条调用路径（都从 `reserved` 出发），同一事务，参数不同**：

| 路径 | 触发 | `outcome` → reservation / attempt / request |
| --- | --- | --- |
| **正常关单** | 终帧到达 | `settled`(actual=真实 usage) / `completed` / `completed` |
| **恢复扫描**（§4.2bis） | 租约超时 | 见 §4.2bis 分流表（三种 outcome） |

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
SELECT gateway_client_id, spend_date, actual_usd
  INTO :cid, :date, :old_actual
  FROM client_reservations
 WHERE request_id = :rid AND state = 'settled'
   FOR UPDATE;                                   -- 未命中（不存在/仍 reserved）→ 报错回滚
-- (2) 幂等闸门：event_key 冲突即无操作
INSERT INTO reservation_adjustments(event_key, request_id, old_actual_usd, new_actual_usd, operator, reason)
VALUES (:event_key, :rid, :old_actual, :new_actual, :operator, :reason)
ON CONFLICT (event_key) DO NOTHING;
-- (3) 仅当 (2) 影响行数 = 1 才执行（否则 COMMIT 返回，聚合不动）
UPDATE client_daily_spend
   SET settled_usd = settled_usd + (:new_actual - :old_actual)   -- 差额，且 old 来自锁定行
 WHERE gateway_client_id = :cid AND spend_date = :date;          -- 二者均派生自 reservation
UPDATE client_reservations
   SET actual_usd = :new_actual, needs_manual_review = false
 WHERE request_id = :rid;
COMMIT;
```

- **`new_actual_usd` 必须 ≥ 0**：该列用 `nonneg_usd` 域（带 `CHECK (VALUE >= 0)`），API 层另行返回 **400** 拒绝负值（不依赖数据库报错）。修正后 `settled_usd` 亦须 ≥ 0，否则整事务回滚。
- **差额修正而非覆盖**：`settled_usd += (new − old)`，配合 `event_key` 幂等键，重复提交与崩溃重放都只生效一次。
- **`cid` / `spend_date` / `old_actual` 三者全部从 `FOR UPDATE` 锁定的 reservation 行派生**，不接受调用方传参——既挡住并发丢失更新，也挡住改错客户、改错日期。跨日核对因此天然记到原始那一天。
- **禁止直接改 `client_daily_spend`**：所有聚合修改只有 `finalize` 与 `adjust` 两个入口。
- **验收**：见 [AC-33](./14-acceptance-matrix.md) ⑭／⑮／⑱／⑲（成功修正 / 重复 event_key 与崩溃重放 / **两个不同 event_key 并发修正后聚合与 reservation 必须一致** / 参数不可指定 client 与日期）。

> **验收**：[AC-33](./14-acceptance-matrix.md) 须含 **kill-after-apply-before-ack** 重放测试：在"已更新聚合表、未标记 outbox delivered"之间 kill，重启重放后 `settled_usd` **不得翻倍**、`reserved_usd` **不得为负**。[AC-35](./14-acceptance-matrix.md) 须在**四个崩溃时点各断言** `client_reservations.state ≠ 'reserved'` 且 `client_daily_spend.reserved_usd` 已归零（无泄漏）。

**鉴权与预留的执行顺序**（第 9 轮 [critical] 修正）：

> ⚠️ 上一版把"校验 `quota_daily_usd`"写在鉴权阶段（selector 之前），但 `estimated_usd` **必须等 selector 产出完整 RoutePlan 才算得出来**——顺序上不可能在那时校验。照原文实现必然写成 check-then-act：多实例同时通过基于旧余额的检查，再各自预留，配额直接被突破（违反 AC-33⑥⑬）。故拆成**快速拒绝**与**原子预留**两段。

| 阶段 | 位置 | 动作 |
| --- | --- | --- |
| **A. 鉴权** | `protocol` 层，最先 | ① `Authorization: Bearer <token>` → 按 `secret_prefix` 定位 → 校验 `secret_hash`；② `status='active'` 且未过期，否则 **401**；③ 模型别名 ∈ `allowed_aliases`，否则 **403** |
| **B. RPM 原子闸** | 同上，鉴权后 | 见下方原子语句；返回 0 行即 **429**。RPM 不依赖 RoutePlan，可在此完成 |
| **C. 日配额快速拒绝** | 同上 | 读内存快照：若 `settled_usd + reserved_usd ≥ quota_daily_usd` 直接 **429**。**这是优化不是保证**——只为省掉必然失败的调度开销，正确性由 D 承担 |
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
```

```sql
-- D. 日费用原子预留：限额判断 + reserved_usd 递增 + reservation 插入，同一事务
BEGIN;
INSERT INTO client_daily_spend(gateway_client_id, spend_date)
VALUES (:cid, :date) ON CONFLICT DO NOTHING;              -- 保证当日行存在

WITH claimed AS (
  UPDATE client_daily_spend
     SET reserved_usd = reserved_usd + :est
   WHERE gateway_client_id = :cid AND spend_date = :date
     AND reserved_usd + settled_usd + :est <= :quota      -- ⚠️ 判断与递增在**同一条** UPDATE 里
  RETURNING gateway_client_id
)
INSERT INTO client_reservations(request_id, gateway_client_id, spend_date, estimated_usd, state)
SELECT :rid, :cid, :date, :est, 'reserved' FROM claimed;  -- claimed 为空则不插入
COMMIT;
-- 插入 0 行 → 整体回滚语义（reserved_usd 也未加）→ 429
```

- `quota_daily_usd IS NULL`（不设日配额）时**跳过 D 的限额判断**，仍插入 reservation 行（记账与崩溃恢复需要它），`reserved_usd` 照常累加。
- **D 必须在发起上游调用前完成**，与 [§5.1 意图先行](./01-architecture.md) 的 attempt 落库同属"发请求前的同步写"。

**签发与轮换**（[09](./09-admin-api.md) 管理 API）：签发时生成随机明文 → 存哈希 → **明文只返回一次**；吊销即置 `status='revoked'` 并记 `revoked_at/revoke_reason`；轮换 = 新签发 + 旧的宽限期后吊销。

**服务 FR/AC**：FR-094（不展示完整凭证）、FR-113（**上游 Key 明文 ≠ 入站凭证明文**，入站强制哈希）；新增 AC 见 [14](./14-acceptance-matrix.md)。

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

- **不可覆盖**：两表均仅 INSERT。「当前生效价」= 该 `(channel,model)` 下 `effective_at<=now()` 的最新 `price_versions` 行；「当前生效倍率」= 该 `binding_id` 下最新 `multiplier_versions` 行。
- **成本计算**：`成本 = 基础价 × 倍率 × token`；**两个版本 id 都必须落到 attempt**，历史复算时同时取回才能还原当时的完整计价输入（FR-013/AC-02）。
- 价格过期/查询失败保守处理（FR-015）由决策层用 `queried_at` 判新鲜度（§11 默认 6h/24h/48h），不在本表建标志位。

**索引**

```sql
CREATE INDEX idx_price_cur ON price_versions(channel_id, model_id, effective_at DESC);
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
  price_version_id  UUID REFERENCES price_versions(id),      -- 决策时的**基础价**版本（FR-013/AC-02）
  multiplier_version_id UUID REFERENCES multiplier_versions(id), -- 决策时的**倍率**版本（binding 级）
  -- 两者合起来才是该 attempt 的完整计价输入，缺一不可复算
  role              TEXT NOT NULL CHECK (role IN ('primary','takeover','retry','canary','probe')), -- FR-076/079
                    -- canary：一期受控验证——**本来就要发的真实业务请求**被分给待验证 binding（不额外产生费用）
                    -- probe：二期主动测活——**为探测而额外发起**的请求（[05 §2](./05-scheduling-and-operations.md) 二期）

  -- ── 状态：网关原始 vs 归并（AC-30 核心）──
  gateway_status    TEXT CHECK (gateway_status IN ('pending','completed','failed','canceled')), -- beta5 execution.status 原样
  error_message     TEXT,                     -- beta5 execution.errorMessage 原文（元数据，非正文）
  -- 归并后的取消/失败口径：beta5 canceled 仅指客户端取消，上游断开/内部超时归 failed
  -- → 自研层按 errorMessage 归并（AC-30、假设2 语义澄清）
  cancel_reason     TEXT CHECK (cancel_reason IN
                      ('none','client_disconnect','sla_takeover','upstream_disconnect','internal_timeout','upstream_error')),
  attempt_status    TEXT NOT NULL DEFAULT 'pending'
                      CHECK (attempt_status IN (
                        -- 非终态（两者都须被 §4.2bis 租约扫描覆盖）
                        'pending',            -- 已落意图，未提交
                        'committed',          -- 已提交输出给下游，流未结束
                        -- 终态
                        'completed','failed','canceled_by_sla','unknown_billing','interrupted')),
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
  -- ── 上游事件到达时刻（**同步写之前**）：与下游交付时刻配对，度量我们自己的开销 ──
  upstream_first_actionable_at TIMESTAMPTZ,   -- 上游首个 ShouldCommit 事件**到达**时刻（早于 response_committed_at）
  upstream_terminal_at         TIMESTAMPTZ,   -- 上游终帧**到达**时刻（早于 outbox 提交）

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
  gateway_reported_ttft_ms INTEGER,           -- beta5 metricsFirstTokenLatencyMs：仅存证、永不采信（AC-31）
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

  -- ── 上游关联键（自研直连，旁路观察提取）──
  upstream_response_id    TEXT,               -- 上游响应 id（如 resp_.../chatcmpl-...），用于排障关联

  started_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  ended_at          TIMESTAMPTZ,

  -- ── attempt 租约（§4.2bis 悬挂检测）──
  external_call_started_at TIMESTAMPTZ,       -- 上游请求实际发出的时刻；NULL=尚未发出（崩溃则未计费）
  lease_heartbeat_at TIMESTAMPTZ,             -- 持有实例的心跳；**插入 attempt 时即写入非 NULL**（防漏扫）
  lease_owner       TEXT,                     -- 持有该 attempt 的实例标识

  PRIMARY KEY (id, request_created_at)
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
-- ⚠️ 三个 WHERE 条件各修过一个 critical 缺陷，缺一即产生永久悬挂：
-- (1) 状态必须含 'committed'：首字后崩溃的 attempt 已不是 pending，
--     只扫 pending 会让它**永远扫不到**（对抗性审查第 7 轮 critical）。
-- (2) 不可只写 external_call_started_at IS NOT NULL，
--     否则"上游调用前崩溃"的 attempt 永远扫不到（第 5 轮 critical）。
-- (3) 心跳判定必须含 IS NULL：SQL 的 < 对 NULL 恒不成立，
--     单写 lease_heartbeat_at < cutoff 会漏掉心跳未写入即崩溃的行（第 5 轮 critical）。
SELECT * FROM attempts
WHERE attempt_status IN ('pending','committed')          -- 两个非终态全覆盖
  AND (lease_heartbeat_at IS NULL
       OR lease_heartbeat_at < now() - INTERVAL '60 seconds')
FOR UPDATE SKIP LOCKED;
```

> **续租义务**：`executor` 在 attempt 存活期间（含 `committed` 后的流式传输阶段）**必须每 ≤20s 续写一次 `lease_heartbeat_at`**。否则长流式响应会被恢复扫描误判为崩溃并强行终结。20s 续租 / 60s 超时 = 3 倍余量。

捞出后**按"上游发出了吗、见到首字了吗"两个事实分流**，每种情形都推到**终态**并在**同一个 `finalize` 事务**（[§2bis](#2bis-网关调用方凭证域入站鉴权对抗性审查新增)）内终结配额预留：

| # | 判据 | attempt 终态 | request 终态 | reservation | 告警 |
| --- | --- | --- | --- | --- | --- |
| ① | `pending` 且 `external_call_started_at IS NULL` | `failed` | `failed` | `abandoned`（**释放预留**，`settled_usd` 不加） | 无（确定未计费） |
| ② | `pending` 且 `external_call_started_at IS NOT NULL` | `unknown_billing` | `failed` | `settled`，`actual_usd` = **全跳汇总**（见下），`needs_manual_review=true` | **P2** |
| ③a | `committed`、`terminal_event IS NOT NULL` 且 `downstream_write_completed_at IS NOT NULL` | `completed`（`terminal_event='error'/'incomplete'` → `failed`） | 同左 | `settled`；`attempt_usage` 已落则用**实际值**、`needs_manual_review=false`，否则估算 + 待核对 | 无／P3 |
| ③c | `committed`、`terminal_event IS NOT NULL` 但 **`downstream_write_completed_at IS NULL`**（上游结果已知，**交付未确认**） | `completed` | **`interrupted`** | `settled`，用**实际值**，`needs_manual_review=false` | 无 |
| ③b1 | `committed`、`terminal_event IS NULL` 且 **`downstream_first_byte_written_at IS NULL`**（= K2，我们一个字节都没写出去） | `interrupted` | **`failed`** | `settled`，全跳汇总，`needs_manual_review=true` | **P3** |
| ③b2 | `committed`、`terminal_event IS NULL` 且 **`downstream_first_byte_written_at IS NOT NULL`**（= K3，写出过部分内容） | `interrupted` | **`interrupted`** | `settled`，全跳汇总，`needs_manual_review=true` | **P3** |

> ⚠️ 三个事实、三次判定，**任何一个都不能由 `attempt_status` 反推**：
> ① `terminal_event` 分 ③a/③c 与 ③b；② `downstream_write_completed_at` 分 ③a 与 ③c；③ `downstream_first_byte_written_at` 分 ③b1 与 ③b2。
> 第 12 轮 [high]：上一版 ③b 只读 `terminal_event`，把「首字尚未写出」（K2）也判成 `interrupted`，等于**声称用户看到过截断流**——实际他什么都没收到。这会虚增 `stream_break_rate` 并污染可用性统计，不可用 `attempt_status='committed'` 推导——空终态与错误终态同样会把 attempt 推到 `committed`（[03 §3.2](./03-upstream-layer.md)）。混为一谈会把正常完成的空响应写成 `interrupted` 并误报人工核对。

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
   | **用户侧 SLA**（按 `requests` 算） | 有效可用性、错误预算、告警 | **计入失败**（`final_status='failed'`） | **计入失败 + `stream_break_rate` 分母**（`final_status='interrupted'`，AC-12/FR-071） |
   | **渠道健康**（按 `attempts` 算，喂 selector） | 冷却、样本门槛、排序 | **不计入**该 binding 成功率 | **不计入**该 binding 成功率 |
   | 成本 | 费用统计、配额 | 计入（估算值） | 计入（估算值） |
   | TTFT | 首字统计 | 无 TTFT（未见首字） | **已记录的 `content_aware_ttft_ms` 照常计入**（那是真实观测值，用户真的等到了首字） |

   > **为什么两套口径方向相反**：进程崩溃是**我们的**故障，不是渠道的故障。计入用户 SLA 是诚实（用户确实失败了）；不计入渠道健康是准确（否则会冤枉一个健康渠道、把它冷却掉，故障范围反而扩大）。
   > **实现**：`resource_health` 的成功率/样本计数按 `attempt_status NOT IN ('unknown_billing','interrupted')` 过滤；SLA 聚合按 `requests.final_status` 算，**不过滤**。

> ②③b 的 `actual_usd` 含估算成分，故 `needs_manual_review=true`；运维据告警核对上游账单后走 `POST /admin/reservations/{request_id}/adjust`——它是[§2bis 的**独立 `adjust` 事务**](#2bis-网关调用方凭证域入站鉴权对抗性审查新增)（从 `settled` 出发、`FOR UPDATE` 锁定、按差额修正），**不是 `finalize`**：`finalize` 的闸门是 `state='reserved'`，对已 `settled` 的待核对行必然影响 0 行、静默失效。幂等键用 `reservation_adjustments.event_key`，**不是** `settle_event_key`。**禁止直接改聚合表**。

**验收**：[AC-35](./14-acceptance-matrix.md) 逐一断言四个崩溃时点的 `attempt_status` + `final_status` + `client_reservations.state` + `client_daily_spend.reserved_usd` **四项全部终结**。

```sql
-- 索引须覆盖两个非终态与心跳为 NULL 的行
CREATE INDEX idx_attempts_stale_lease ON attempts(attempt_status, lease_heartbeat_at NULLS FIRST)
  WHERE attempt_status IN ('pending','committed');
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
  prompt_tokens     INTEGER,                  -- beta5 usageLogs.promptTokens
  completion_tokens INTEGER,                  -- completionTokens
  total_tokens      INTEGER,                  -- totalTokens
  prompt_cached_tokens INTEGER,               -- promptCachedTokens（缓存部分，FR-054）
  total_cost        nonneg_usd,               -- beta5 usageLogs.totalCost
  cost_items        JSONB,                    -- beta5 usageLogs.costItems（明细，元数据）
  cost_source       TEXT NOT NULL DEFAULT 'upstream' CHECK (cost_source IN ('upstream','estimated')),
  -- 预估 vs 实际扣费差异（FR-016/019）：超容差标计费异常
  estimated_cost    nonneg_usd,
  cost_variance     usd_amount,               -- 实际-预估；**可为负**（实际低于预估），故用可正负的 usd_amount（FR-019/AC-23）
  PRIMARY KEY (id, request_created_at),       -- 分区表：主键必须包含分区键
  -- 指向 attempts 的复合外键（分区表间引用须带分区键）
  FOREIGN KEY (attempt_id, request_created_at)
      REFERENCES attempts (id, request_created_at)
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
  UNIQUE (binding_id, model_id, request_type, context_bucket, window_kind, window_start)
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
  -- 配额状态（FR-118、假设5）：未知默认保守排除（自研层补，AxonHub 默认保留）
  quota_status    TEXT CHECK (quota_status IN ('available','warning','exhausted','unknown')),
  -- ↑ 对齐 beta5 Channel.providerQuotaStatus；unknown → selector 默认排除（FR-118）
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

**索引**

```sql
CREATE INDEX idx_cred_channel     ON collector_credentials(channel_id, status);
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
  category        TEXT NOT NULL,               -- error_budget/sla_breach/balance/key_invalid/price_anomaly/sub_expiry/capacity/fault_domain/model_capability/data_stale
  scope           JSONB,                       -- 影响范围（渠道/资源/故障域）
  trigger_data    JSONB,                       -- 触发数据（元数据）
  suggested_action TEXT,                        -- 建议处置（FR-101）
  state           TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open','acknowledged','recovering','closed')),
  started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  acknowledged_at TIMESTAMPTZ,
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

> 生命周期转换（open→acknowledged→recovering→closed）须在**行锁**下进行（`SELECT … FOR UPDATE`），
> 避免并发转换产生第二条活动行。如需保留"同因重复发生"的明细，另建 `alert_occurrences` 子表，
> 不在主表堆积。
```

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

### 9.2 保留策略配置化

- 保留窗口（默认 7 个月 ≥180 天）走 `config_params(param_key='retention.months', is_critical=true)`；缩短保留是关键操作，需二次确认（FR-115）。
- **不可存列的守卫**：CI 中加 schema 断言，禁止任何账本表出现 `body/messages/prompt/completion_text/headers` 命名列（FR-112 硬约束 3）。

### 9.2bis 账本 outbox（防崩溃丢账，[01 §5.1](./01-architecture.md)）

账本状态更新不得只存在于内存队列——进程崩溃即永久丢账（账本是唯一真相源，无处可对账）。异步更新一律先写 outbox（与业务行同事务），再由后台投递：

```sql
CREATE TABLE ledger_outbox (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  attempt_id    UUID NOT NULL,
  request_created_at TIMESTAMPTZ NOT NULL,     -- 用于定位分区
  event_type    TEXT NOT NULL CHECK (event_type IN
                  ('first_token','attempt_end','usage','cancel','request_close')),
  payload       JSONB NOT NULL,                -- 状态更新的元数据（不含正文，FR-112）
  -- 幂等键：同一 attempt 的同一状态跃迁只生效一次，重放不产生重复/不覆盖更晚状态
  idempotency_key TEXT NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  delivered_at  TIMESTAMPTZ,                   -- NULL = 待投递
  UNIQUE (idempotency_key)
);

CREATE INDEX idx_outbox_pending ON ledger_outbox(created_at) WHERE delivered_at IS NULL;
```

**恢复流程**：实例启动时扫描 `delivered_at IS NULL` 的行并重放（`FOR UPDATE SKIP LOCKED`，多实例安全）。因幂等键存在，重放不会产生重复账目。

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
| §2bis 入站凭证与配额（gateway_clients/client_daily_spend/client_rate_window/client_reservations/**reservation_adjustments**） | FR-094、113、120 | AC-33 |
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
| 5 | 隐藏重试补算的触发点 | ⛔ **已废止（2026-07-25）**：原方案依赖外部网关审计比对，转向后**无网关、无对账阶段**。当前结论：**一期不做隐藏重试推断**——我们自己不发隐藏重试（FR-119），`attempt_usage.upstream_seq` 恒为 1；上游中转站内部若有隐藏重试，我们**不可观测也不补算**，其成本已体现在上游回传的 usage 里 |
| 6 | 一期入站协议范围（FR-111 现为 CC + Responses） | ✅ **已定（2026-07-23）：维持 CC + Responses，不扩** —— **主力客户端为 Codex CLI，说的正是 OpenAI Responses，已在一期范围内**。Claude Code（Anthropic Messages）与 Gemini CLI（Gemini API）一期不直连；提取规则保留在 §4.5 仅为将来扩协议时零改动 |

---

_本篇为 M0 前的数据模型基线。任何字段变更须回溯到具体 FR/AC 或 ISSUE-001/002 的运行时事实，不得凭空增列；账本表新增列前须过 FR-112「不存正文」CI 断言。_
