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
CREATE DOMAIN usd_amount AS NUMERIC(20,10);

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
  -- ⚠️ 与上游 Key 不同：入站凭证**只存哈希**（Argon2id/bcrypt），明文仅在签发时返回一次
  secret_hash   TEXT NOT NULL,
  secret_prefix TEXT NOT NULL,                 -- 明文前 8 位，供展示与定位（如 "gw-a1b2c3"）
  -- 授权范围
  allowed_aliases TEXT[],                      -- 可用的模型别名；NULL=全部（FR-062/117）
  -- 配额（防单个调用方烧光额度）
  quota_daily_usd usd_amount,                  -- 日费用上限；NULL=不限
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
  reserved_usd  usd_amount NOT NULL DEFAULT 0,  -- 已预留（请求发起时 +预估）
  settled_usd   usd_amount NOT NULL DEFAULT 0,  -- 已结算（请求结束时按实际替换预留）
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

**预留—结算协议**（防"落账前并发超限"）：

| 时点 | 动作 |
| --- | --- |
| 请求进入 | 单事务内 `INSERT … ON CONFLICT DO UPDATE SET reserved_usd = reserved_usd + <预估>` 并**在同一语句返回新值**；超 `quota_daily_usd` 即回滚并 **429** |
| RPM 校验 | 同事务 `UPDATE client_rate_window SET request_count = request_count + 1` 并判定上限；超限 **429** |
| 请求结束 | 用实际费用替换预留：`reserved_usd -= 预估`、`settled_usd += 实际` |
| 崩溃兜底 | 悬挂 attempt（§4.2bis）转 `unknown_billing` 时，其预留**保留不释放**（保守），由运维核对后修正 |

> 关键点：**预留发生在请求发起前、且与检查在同一事务** —— 这才挡得住"两个并发请求都读到旧余额"。仅在结束时累加实际费用是挡不住的。

**鉴权流程**（`protocol` 层，先于任何调度）：

1. 取 `Authorization: Bearer <token>` → 按 `secret_prefix` 定位候选行 → 校验 `secret_hash`。
2. 校验 `status='active'` 且未过期 → 否则 **401**（已吊销/过期）。
3. 校验请求的模型别名 ∈ `allowed_aliases` → 否则 **403**（越权）。
4. 校验 `rpm_limit` 与 `quota_daily_usd` → 超限 **429**。
5. 通过后才进入 policy/selector；`requests.tenant_id` 取自该凭证。

**签发与轮换**（[09](./09-admin-api.md) 管理 API）：签发时生成随机明文 → 存哈希 → **明文只返回一次**；吊销即置 `status='revoked'` 并记 `revoked_at/revoke_reason`；轮换 = 新签发 + 旧的宽限期后吊销。

**服务 FR/AC**：FR-094（不展示完整凭证）、FR-113（**上游 Key 明文 ≠ 入站凭证明文**，入站强制哈希）；新增 AC 见 [14](./14-acceptance-matrix.md)。

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
  input_price     usd_amount NOT NULL,        -- 每计费单位（归一为美元）
  output_price    usd_amount NOT NULL,
  cache_price     usd_amount,                 -- 缓存价（走折扣）
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
                  CHECK (final_status IN ('pending','completed','failed','canceled','unavailable')),
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
  role              TEXT NOT NULL CHECK (role IN ('primary','takeover','retry','probe')), -- FR-076/079

  -- ── 状态：网关原始 vs 归并（AC-30 核心）──
  gateway_status    TEXT CHECK (gateway_status IN ('pending','completed','failed','canceled')), -- beta5 execution.status 原样
  error_message     TEXT,                     -- beta5 execution.errorMessage 原文（元数据，非正文）
  -- 归并后的取消/失败口径：beta5 canceled 仅指客户端取消，上游断开/内部超时归 failed
  -- → 自研层按 errorMessage 归并（AC-30、假设2 语义澄清）
  cancel_reason     TEXT CHECK (cancel_reason IN
                      ('none','client_disconnect','sla_takeover','upstream_disconnect','internal_timeout','upstream_error')),
  attempt_status    TEXT NOT NULL DEFAULT 'pending'
                      CHECK (attempt_status IN ('pending','committed','canceled_by_sla','failed','completed',
                                                'unknown_billing')),
                      -- ⚠️ unknown_billing：上游请求**已发出**但进程在写入首条 outbox 事件前崩溃，
                      -- 无事件可重放、账本无外部对账源 → 该调用**可能已计费但结果不明**。
                      -- 必须显式标记而非静默留 pending（否则永久悬挂、污染成本与 SLA 统计）。见 §4.2bis

  -- ── 逐尝试 metrics（自算，AC-31）──
  content_aware_ttft_ms INTEGER,              -- 自算内容感知首字（排除 role-only/空SSE/心跳，AC-31/假设3/6）
  gateway_reported_ttft_ms INTEGER,           -- beta5 metricsFirstTokenLatencyMs：仅存证、永不采信（AC-31）
  full_latency_ms   INTEGER,                  -- 总延迟（请求进入→完整结束）
  upstream_latency_ms INTEGER,                -- 上游耗时（发往上游→完整结束）
  -- 决策/网关自身开销 = full_latency_ms − upstream_latency_ms，用于 FR-110「P99≤50ms」自监控（测法见 06 §6）
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
> `lease_heartbeat_at` 在 ① 即写入非 NULL，**杜绝"已发出但无心跳"的漏扫**。

**恢复扫描**（启动时 + 周期性，与 outbox 重放并列）——**必须覆盖两类，缺一即产生永久 pending**：

```sql
-- ⚠️ 一条 SQL 同时捞两类：不可只写 external_call_started_at IS NOT NULL，
--    否则"上游调用前崩溃"的 attempt 永远扫不到（对抗性审查 critical 发现）。
-- ⚠️ 心跳判定必须含 IS NULL：SQL 的 < 对 NULL 恒不成立，
--    单写 lease_heartbeat_at < cutoff 会漏掉心跳未写入即崩溃的行。
SELECT * FROM attempts
WHERE attempt_status = 'pending'
  AND (lease_heartbeat_at IS NULL
       OR lease_heartbeat_at < now() - INTERVAL '60 seconds')
FOR UPDATE SKIP LOCKED;
```

捞出后**按是否已发出上游请求分流**：

| 情形 | 处置 |
| --- | --- |
| `external_call_started_at IS NULL` | 上游**未发出** → 置 `failed`，**确定未计费**，无需告警 |
| `external_call_started_at IS NOT NULL` | **可能已计费** → 置 **`unknown_billing`** + 触发运维告警（[§8](#8-告警与事件域合并持续事件) `alert_events`，P2） |

→ `unknown_billing` 的 attempt **计入成本上限的保守估算**（按该 binding 的价格版本 × 倍率版本估一次调用费用），但**不计入成功率统计**；运维据告警人工核对上游账单后修正。

**验收**：[AC-35](./14-acceptance-matrix.md) 覆盖四个崩溃时点，其中"上游发出后、首条 outbox 前"须得到 `unknown_billing` + 告警，"上游调用前"须得到 `failed`。

```sql
-- 索引须覆盖两类扫描（含心跳为 NULL 的行）
CREATE INDEX idx_attempts_stale_lease ON attempts(attempt_status, lease_heartbeat_at NULLS FIRST)
  WHERE attempt_status = 'pending';
```

### 4.3 逐 Attempt 用量与费用

```sql
-- attempt 级用量/费用（FR-058、FR-016/019；假设4 token+cost 已收口）
-- 隐藏重试可能对应多条上游用量 → 用 attempt_id + upstream_seq 表达（FR-119）
CREATE TABLE attempt_usage (
  id                UUID NOT NULL,            -- UUIDv7（分区表主键须含分区键，见表尾复合 PK）
  attempt_id        UUID NOT NULL,
  request_created_at TIMESTAMPTZ NOT NULL,    -- 冗余分区键
  upstream_seq      SMALLINT NOT NULL DEFAULT 1, -- 第几次上游调用（隐藏重试时 >1，FR-119）
  prompt_tokens     INTEGER,                  -- beta5 usageLogs.promptTokens
  completion_tokens INTEGER,                  -- completionTokens
  total_tokens      INTEGER,                  -- totalTokens
  prompt_cached_tokens INTEGER,               -- promptCachedTokens（缓存部分，FR-054）
  total_cost        usd_amount,               -- beta5 usageLogs.totalCost
  cost_items        JSONB,                    -- beta5 usageLogs.costItems（明细，元数据）
  cost_source       TEXT NOT NULL DEFAULT 'upstream' CHECK (cost_source IN ('upstream','estimated')),
  -- 预估 vs 实际扣费差异（FR-016/019）：超容差标计费异常
  estimated_cost    usd_amount,
  cost_variance     usd_amount,               -- 实际-预估；无法归因差额单列（FR-019/AC-23）
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
  fixed_fee       usd_amount NOT NULL,
  currency        TEXT NOT NULL DEFAULT 'USD',-- 一期仅名称（FR-018）
  -- 有效期（FR-033）：sub2api validity_days×unit / ASXS durationDays
  validity_days   INTEGER,
  billing_period  TEXT,                       -- daily/weekly/monthly（ASXS limits.limitType + windowMode=fixed）
  -- 周期包含额度（FR-033）：ASXS limits.limitMicros / sub2api group.*_limit_usd
  period_quota    usd_amount,
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
  used_quota      usd_amount,                 -- ASXS usedMicros/1e6
  left_quota      usd_amount,                 -- ASXS leftMicros/1e6
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
  limit_usd       usd_amount,                 -- *_limit_usd（周期上限）
  usage_usd       usd_amount,                 -- *_usage_usd（已用）
  window_start    TIMESTAMPTZ,                -- *_window_start
  window_resets_at TIMESTAMPTZ,               -- *_window_resets_at（重置时间 → FR-036 到期未用预测）
  fetched_at      TIMESTAMPTZ NOT NULL,
  UNIQUE (subscription_id, window_kind)
);

-- 到期未用额度预测（FR-036/039、8.5；AC-20/21/23）
CREATE TABLE subscription_waste_forecast (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  subscription_id BIGINT NOT NULL REFERENCES user_subscriptions(id),
  predicted_wasted_quota usd_amount,          -- max(0, 剩余 - 到期前预计合格消耗)
  eligible_demand usd_amount,                 -- 合格业务需求预估
  recent_consumption_rate usd_amount,         -- 近期消耗速度
  active_reset_gain usd_amount,               -- 可主动重置带来的外生额度（FR-036 外生变量）
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
                    CHECK (health_state IN ('unknown','observing','available','degraded','cooling','disabled')),
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
  last_confirmed_balance usd_amount,            -- 归一美元
  confirmed_at    TIMESTAMPTZ,
  known_consumption_since usd_amount,           -- 自确认点后的已知消耗（形成保守下限）
  conservative_floor usd_amount,                -- = last_confirmed - known_consumption
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
| §3 价格版本（price_versions/price_change_log） | FR-010、012~018 | AC-02/03/17 |
| §4 请求与 Attempt 账本（requests/attempts/attempt_usage/session_prefix_ledger） | FR-040、050/051、058、070~072、076/078/079/080、092、097~099、112、116、**119** | AC-06/07/09/12/13/16、**AC-30/31/32** |
| §5 订阅台账（subscription_plans/user_subscriptions/quota_windows/waste_forecast） | FR-011、033~039、057/058 | AC-20/21/22/23/**24** |
| §6 健康/冷却/样本（resource_health/health_metric_windows/quality_events） | FR-007、040~047、060/065/066 | AC-08/10 |
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
| 3 | AxonHub 换 PG 共库 vs 独立 SQLite（01-架构开放点4） | ✅ **已实测**（[07 §1](./07-axonhub-runtime-probes.md)）：beta5 支持 PG；用 DSN `search_path=<schema>`（需预建）与自研账本**共库分 schema**，对账本地 JOIN、免跨库 |
| 4 | `decision_snapshot` JSONB 体积（每请求一份候选/排除快照） | ✅ **已定**：元数据裁剪 + 仅存 binding_id 与原因码，不存完整对象；必要时挪冷分区压缩 |
| 5 | 隐藏重试补算的触发点 | ✅ **已定**：对账阶段按渠道 `site_family`/channel_type 与网关审计比对推断（ccLoad Codex 渠道），非请求路径实时判 |
| 6 | 一期入站协议范围（FR-111 现为 CC + Responses） | ✅ **已定（2026-07-23）：维持 CC + Responses，不扩** —— **主力客户端为 Codex CLI，说的正是 OpenAI Responses，已在一期范围内**。Claude Code（Anthropic Messages）与 Gemini CLI（Gemini API）一期不直连；提取规则保留在 §4.5 仅为将来扩协议时零改动 |

---

_本篇为 M0 前的数据模型基线。任何字段变更须回溯到具体 FR/AC 或 ISSUE-001/002 的运行时事实，不得凭空增列；账本表新增列前须过 FR-112「不存正文」CI 断言。_
