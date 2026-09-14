CREATE DOMAIN usd_amount AS NUMERIC(20,10);

CREATE DOMAIN nonneg_usd AS NUMERIC(20,10) CHECK (VALUE >= 0);

CREATE TABLE upstream_providers (
  id      BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name    TEXT NOT NULL,          -- 如 openai/anthropic 官方供应商
  note    TEXT
);

CREATE TABLE channels (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name            TEXT NOT NULL,
  -- 取值范围 = 站型注册表的家族 + unknown 哨兵（04 §7bis）。加一族要配一条迁移
  -- 放宽它，否则新族的渠道**建不进来**（约束冲突，报在写入时而不是编译时）。
  site_family     TEXT NOT NULL CHECK (site_family IN ('newapi','sub2api','unknown')),
  -- 全库唯一（019）：三处写入路径（createChannel / importOne / patchChannel）都是
  -- "先查再插/改"，而 read-then-insert **防不住并发** —— 两个并发导入或一次 201
  -- 丢失后的重试都能各插一条，台账里出现重复渠道而库里没有 DELETE 入口。
  -- 约束认字面值，故规范化是写入侧的责任：internal/admin.validateBaseURL
  -- 校验（scheme 为 http/https 且 host 非空）**并返回规范形态**（去首尾空白、
  -- 小写 host、去尾斜杠）—— 三处入口共用它，少一处就能用一个 "/" 或一个大写字母
  -- 绕过本约束。只小写 host 不动 path：path 大小写敏感。
  -- ⚠️ 019 文件头里那句"没做 lower()…这个残留缺口记在这里"**已过时，且改不了** ——
  -- 019 已应用到真库，checksum 契约把整个文件（含注释）冻住了，改注释也会让
  -- 迁移在启动时直接 return error。2026-09-01 实测撞过一次：只改了 019 的注释，
  -- remote-stack.sh 起 core 就报"已应用但内容已变"。**以本处为准。**
  base_url        TEXT NOT NULL UNIQUE,
  upstream_provider_id BIGINT REFERENCES upstream_providers(id),   -- 真实上游（故障域根，FR-044）
  status          TEXT NOT NULL DEFAULT 'enabled' CHECK (status IN ('enabled','disabled')), -- FR-004
  disabled_reason TEXT,           -- FR-095 人工停用原因
  disabled_until  TIMESTAMPTZ,    -- FR-095 有效期
  catalog_sync_seq BIGINT NOT NULL DEFAULT 0 CHECK (catalog_sync_seq >= 0), -- 可靠目录成功轮次（FR-126）
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE upstream_accounts (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  channel_id    BIGINT NOT NULL REFERENCES channels(id),
  external_user_id TEXT,          -- NewAPI 数字用户ID（New-API-User 头必需，ISSUE-002 §3.1）
  balance_group_key TEXT,         -- 共享余额分组键：同 key 的多账号/多Key 只算一次余额（FR-022/AC-04）
  -- 027 补：账号在上游的默认分组名（/api/user/self 的 group）。
  -- Key 的 group 为空串时调用走的是它，所以那种 Key 不是"未归组"而是"跟账号走"。
  -- 存名字不存外键：实测有站点的账号分组（default）不在它自己的 group_ratio 里，
  -- 存外键只能写 NULL，把"上游说是 default、我们没采到它的倍率"这个事实丢掉。
  account_group TEXT,             -- NULL = 未采到，**不可当成 'default'**
  -- 人工停用（五层停用开关之一）
  status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
  disabled_reason TEXT,
  disabled_until  TIMESTAMPTZ,    -- NULL = 无限期停用，须人工恢复
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

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

CREATE INDEX idx_chmodel_usable ON channel_models(model_id, protocol)
  WHERE enabled AND support = 'supported';

CREATE TABLE cache_scopes (
  id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  channel_id  BIGINT NOT NULL REFERENCES channels(id),
  account_id  BIGINT REFERENCES upstream_accounts(id),
  key_id      BIGINT REFERENCES upstream_keys(id),
  model_id    BIGINT REFERENCES models(id),
  scope_label TEXT NOT NULL      -- 缓存绑定的渠道/账号/Key/模型组合标识
);

CREATE TABLE fault_domains (
  id     BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  kind   TEXT NOT NULL CHECK (kind IN ('provider','proxy','account','region','network')),
  label  TEXT NOT NULL,
  -- 域级停用：selector 排除该域下**全部** binding（一次操作隔离整个故障域）
  disabled_until  TIMESTAMPTZ,
  disabled_reason TEXT,
  UNIQUE (kind, label)
);

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

CREATE TABLE binding_fault_domains (
  binding_id      BIGINT NOT NULL REFERENCES bindings(id),
  fault_domain_id BIGINT NOT NULL REFERENCES fault_domains(id),
  PRIMARY KEY (binding_id, fault_domain_id)
);

CREATE INDEX idx_channels_family      ON channels(site_family) WHERE status='enabled';

CREATE INDEX idx_accounts_balancegrp  ON upstream_accounts(balance_group_key); -- 共享余额只算一次（FR-022）

CREATE INDEX idx_keys_account_status  ON upstream_keys(account_id, status);

CREATE INDEX idx_bindings_channel     ON bindings(channel_id);

CREATE TABLE channel_groups (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  channel_id    BIGINT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  group_ref     TEXT NOT NULL,               -- 上游分组标识（NewAPI group / Sub2API group_id）
  rate_multiplier NUMERIC(12,6),             -- 分组倍率（采集所得；历史版本仍走 multiplier_versions）
  -- 028 补：有一类分组没有自己的固定倍率（NewAPI 的 auto —— 运行时在候选里
  -- 选一个组计费，源码显式排除 auto 自己）。上游照样给它一个倍率，那是展示
  -- 占位不是计费倍率，只存它会报出一个与账单无关却看起来正常的数字。
  -- ⚠️ 三种状态要分得开：固定倍率 / 动态倍率 / 未采到（dynamic=false 且 rate IS NULL）。
  -- ⚠️ 空分组的 Key **不等于**动态：它走账号的默认分组（account_group），
  --    那通常是个有固定倍率的普通组，只有账号分组恰好是 auto 时才落到动态。
  rate_dynamic  BOOLEAN NOT NULL DEFAULT false,
  dynamic_candidates TEXT[],                 -- 候选分组名（auto_groups）；存名字不存外键，候选里可能有我方没采到的组
  data_source   TEXT NOT NULL CHECK (data_source IN ('auto_collect','manual')),
  fetched_at    TIMESTAMPTZ NOT NULL,        -- 陈旧性查询期计算，不存 is_stale（与 §7 同一做法）
  UNIQUE (channel_id, group_ref)
);

CREATE TABLE group_models (
  channel_group_id BIGINT NOT NULL REFERENCES channel_groups(id) ON DELETE CASCADE,
  model_name    TEXT NOT NULL,
  fetched_at    TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (channel_group_id, model_name)
);

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
  last_seen_seq BIGINT NOT NULL DEFAULT 0 CHECK (last_seen_seq >= 0), -- 最近出现的可靠目录轮次
  -- 下面两列由 026 迁移补（2026-09-14 实测 /api/pricing 的 vendors 数组与
  -- 每模型的 supported_endpoint_types）。两者都可空：缺席 = 上游未声明，
  -- **不是**"无供应商"或"不支持任何端点"（同 billing_unit 的口径）。
  vendor_name    TEXT,                       -- 发行方，按 vendor_id 在顶层 vendors[] 里解析出的 name；跨站点不归一
  endpoint_types TEXT[],                     -- 支持的端点类型（openai / anthropic / gemini / image-generation / …）
  PRIMARY KEY (channel_id, model_name)
);

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

CREATE INDEX idx_chgroups_channel ON channel_groups(channel_id);

CREATE INDEX idx_catalog_channel  ON channel_model_catalog(channel_id, last_seen_at DESC);

CREATE INDEX idx_keys_group       ON upstream_keys(channel_group_id);

CREATE UNIQUE INDEX idx_keys_external_ref
  ON upstream_keys (account_id, external_ref) WHERE external_ref IS NOT NULL;

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

CREATE INDEX idx_aliases_policy   ON model_aliases(policy_id) WHERE enabled;

CREATE INDEX idx_slatargets_pol   ON sla_targets(policy_id, model_id);

CREATE INDEX idx_config_active    ON config_params(scope_type, scope_id, param_key) WHERE version IS NOT NULL;

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

CREATE TABLE client_daily_spend (
  gateway_client_id BIGINT NOT NULL REFERENCES gateway_clients(id),
  spend_date    DATE NOT NULL,
  reserved_usd  nonneg_usd NOT NULL DEFAULT 0,  -- 已预留（请求发起时 +预估）
  settled_usd   nonneg_usd NOT NULL DEFAULT 0,  -- 已结算（请求结束时按实际替换预留）
  PRIMARY KEY (gateway_client_id, spend_date)
);

CREATE TABLE client_rate_window (
  gateway_client_id BIGINT NOT NULL REFERENCES gateway_clients(id),
  window_start  TIMESTAMPTZ NOT NULL,           -- 分钟粒度对齐
  request_count INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (gateway_client_id, window_start)
);

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

CREATE INDEX idx_resv_review ON client_reservations(gateway_client_id, spend_date)
  WHERE needs_manual_review;

CREATE INDEX idx_resv_open ON client_reservations(gateway_client_id, spend_date)
  WHERE state = 'reserved';

CREATE TABLE reservation_adjustments (
  event_key     TEXT PRIMARY KEY,              -- 幂等键：重复提交/崩溃重放只生效一次
  request_id    UUID NOT NULL REFERENCES client_reservations(request_id),
  old_actual_usd nonneg_usd NOT NULL,          -- 修正前值（审计）
  new_actual_usd nonneg_usd NOT NULL,
  operator      TEXT NOT NULL,                 -- 责任人（FR-099/104）
  reason        TEXT NOT NULL,                 -- 依据（如"上游账单 #12345"）
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

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

CREATE TABLE price_versions (
  id              UUID PRIMARY KEY,           -- UUIDv7
  channel_id      BIGINT NOT NULL REFERENCES channels(id),
  model_id        BIGINT NOT NULL REFERENCES models(id),
  input_price     nonneg_usd NOT NULL,        -- 每计费单位（归一为美元）
  output_price    nonneg_usd NOT NULL,
  cache_price     nonneg_usd,                 -- 缓存价（走折扣）
  -- billing_unit：可空 + CHECK，与 channel_model_catalog **同一套定义**（018 对齐）。
  -- ⚠️ 原为 `NOT NULL DEFAULT 'per_1m_token'`，与 §1.3bis 的「无价则口径留 NULL、
  --    不补默认值」直接矛盾 —— 而**本表才是成本公式读的那张**。详见 018 头部。
  billing_unit    TEXT CHECK (billing_unit IN
                    ('per_1m_token','per_1k_token','per_token','per_call')),
  currency        TEXT NOT NULL DEFAULT 'USD',-- 一期仅名称，不换汇（FR-018/AC-17）
  data_source     TEXT NOT NULL,              -- auto_collect / manual
  queried_at      TIMESTAMPTZ NOT NULL,       -- 查询时间（FR-012）
  effective_at    TIMESTAMPTZ NOT NULL,       -- 生效时间（FR-012）
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

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

ALTER TABLE price_versions ADD COLUMN confirmed BOOLEAN NOT NULL DEFAULT true;

ALTER TABLE price_versions ADD COLUMN confirmed_by TEXT;

ALTER TABLE price_versions ADD COLUMN confirmed_at TIMESTAMPTZ;

CREATE INDEX idx_price_cur ON price_versions(channel_id, model_id, effective_at DESC)
  WHERE confirmed;

CREATE INDEX idx_mult_cur  ON multiplier_versions(binding_id, effective_at DESC);

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

CREATE INDEX idx_requests_open ON requests(created_at) WHERE final_status = 'pending';

CREATE INDEX idx_attempts_lease ON attempts(request_id, attempt_no, lease_heartbeat_at NULLS FIRST);

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

CREATE INDEX idx_attempts_request   ON attempts(request_id, request_created_at); -- 归集同一请求的多跳

CREATE INDEX idx_attempts_binding   ON attempts(binding_id, started_at);         -- 喂健康统计（§7）

CREATE INDEX idx_attempts_upresp    ON attempts(upstream_response_id);           -- 排障关联上游响应

CREATE INDEX idx_attempts_cancel    ON attempts(cancel_reason)
                                       WHERE cancel_reason<>'none';               -- 取消口径统计（AC-30）

CREATE INDEX idx_usage_attempt      ON attempt_usage(attempt_id, request_created_at);

CREATE INDEX idx_req_session        ON requests(session_id, created_at) WHERE session_id IS NOT NULL;

CREATE INDEX idx_req_tenant_level   ON requests(tenant_id, sla_level, created_at); -- 指标维度（FR-091）

CREATE TABLE subscription_plans (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  channel_id      BIGINT NOT NULL REFERENCES channels(id),
  external_plan_id TEXT,                      -- 上游 planId / group_id（sub2api）
  name            TEXT NOT NULL,              -- 上游 planName / sub2api plan.name（如「每日90刀」）
  -- 固定费用（FR-033）：sub2api plans.price / 自建站的 priceCnyCent，归一为美元
  fixed_fee       nonneg_usd NOT NULL,
  currency        TEXT NOT NULL DEFAULT 'USD',-- 一期仅名称（FR-018）
  -- 有效期（FR-033）：sub2api validity_days×unit / 自建站的 durationDays
  validity_days   INTEGER,
  billing_period  TEXT,                       -- daily/weekly/monthly（上游 limits.limitType + windowMode=fixed）
  -- 周期包含额度（FR-033）：上游 limits.limitMicros / sub2api group.*_limit_usd
  period_quota    nonneg_usd,
  supported_models JSONB,                     -- 支持模型（FR-033）
  -- 倍率（FR-033）：sub2api group.rate_multiplier + 高峰倍率
  rate_multiplier NUMERIC(12,6),
  peak_rate_enabled BOOLEAN DEFAULT false,    -- sub2api group.peak_rate_enabled
  peak_start      TEXT,                       -- peak_start（时段）
  peak_end        TEXT,
  peak_rate_multiplier NUMERIC(12,6),
  reset_rule      TEXT,                       -- 重置规则（FR-033）：固定窗口 日/周/月（sub2api window / 上游 fixedResetTime）
  renewal_status  TEXT,                       -- 续订状态（FR-033）：上游 renewalRule / renewAllowed
  -- 超额计费规则（FR-033）：sub2api 无超额概念 → 据实登记 'no_overage_block'（ISSUE-002 §3.2 结论2）
  overage_rule    TEXT NOT NULL DEFAULT 'unknown'
                    CHECK (overage_rule IN ('no_overage_block','metered','unknown')),

  -- ── 双倍率（参数14/AC-24/FR-057/058）──
  usable_multiplier NUMERIC(12,6),            -- 用满倍率＝固定费用÷周期额度×分组倍率 → 调度排序
  actual_multiplier NUMERIC(12,6),            -- 实际倍率＝固定费用÷实际消耗×倍率 → 账务报表

  -- ── 额度来源优先级（FR-033、术语§3；上游 primarySource/secondarySource）──
  primary_source  TEXT,                       -- 如 'subscription'
  secondary_source TEXT,                      -- 如 'balance' —— 判断订阅额度是否真会被消耗

  -- ── 可主动重置额度（FR-033；上游 dailyReset）：只读登记，不自动触发 ──
  active_reset_supported BOOLEAN NOT NULL DEFAULT false,
  active_reset_threshold_pct NUMERIC(5,2),    -- usageThresholdPercent（如 90）
  active_reset_daily_limit INTEGER,           -- dailyLimit（如 4 次/日）

  data_source     TEXT NOT NULL,              -- auto_collect / manual（FR-011）
  fetched_at      TIMESTAMPTZ NOT NULL,
  valid_until     TIMESTAMPTZ,                -- 人工录入 7 天有效（FR-011）
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE user_subscriptions (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  plan_id         BIGINT NOT NULL REFERENCES subscription_plans(id),
  channel_id      BIGINT NOT NULL REFERENCES channels(id),
  -- 共享额度聚合键（FR-035，源码级确证 UpdateSubscriptionUsage(userID,groupID,cost)）
  ext_user_id     TEXT NOT NULL,              -- sub2api user_id
  group_id        TEXT NOT NULL,              -- sub2api group_id —— 共享额度按 (user_id,group_id) 归集，不按 Key
  starts_at       TIMESTAMPTZ,                -- sub2api starts_at / 上游 startsAt
  expires_at      TIMESTAMPTZ,               -- expires_at / expiresAt（到期时间，FR-033）
  status          TEXT NOT NULL CHECK (status IN ('not_effective','active','expired','suspended','data_unknown')),
                                              -- 对齐 PRD §9.3 订阅状态 + sub2api active/expired/suspended
  -- 已用/剩余（FR-034）：区分订阅额度、现金余额、超额付费
  used_quota      nonneg_usd,                 -- 上游 usedMicros/1e6（micros 类单位在适配器内归一）
  left_quota      nonneg_usd,                 -- 上游 leftMicros/1e6
  remaining_days  INTEGER,                    -- 上游 remainingDays（到期紧迫度，FR-037）
  data_source     TEXT NOT NULL,
  fetched_at      TIMESTAMPTZ NOT NULL,
  valid_until     TIMESTAMPTZ,                -- 人工 7 天有效（FR-011）
  UNIQUE (channel_id, ext_user_id, group_id)  -- 共享额度只计一次（FR-035/AC-22）
);

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

CREATE INDEX idx_plans_channel      ON subscription_plans(channel_id);

CREATE INDEX idx_subs_shared        ON user_subscriptions(ext_user_id, group_id); -- 共享额度聚合（FR-035）

CREATE INDEX idx_subs_expiry        ON user_subscriptions(expires_at) WHERE status='active'; -- 临近到期扫描（FR-037）

CREATE INDEX idx_qwin_reset         ON subscription_quota_windows(window_resets_at);

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

CREATE TABLE quality_events (
  id            UUID PRIMARY KEY,
  binding_id    BIGINT NOT NULL REFERENCES bindings(id),
  request_id    UUID,
  event_type    TEXT NOT NULL CHECK (event_type IN ('empty_response','format_broken','capability_mismatch','suspected_swap')),
  detected_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_health_state    ON resource_health(health_state);

CREATE INDEX idx_health_cooldown ON resource_health(cooldown_until) WHERE health_state='cooling';

CREATE INDEX idx_hwin_binding    ON health_metric_windows(binding_id, window_kind, window_start DESC);

CREATE TABLE cache_hit_windows (
  session_id      TEXT NOT NULL,
  cache_scope_id  BIGINT NOT NULL REFERENCES cache_scopes(id),
  window_start    TIMESTAMPTZ NOT NULL,        -- 小时窗口
  cached_tokens   BIGINT NOT NULL DEFAULT 0,   -- Σ prompt_cached_tokens
  cacheable_tokens BIGINT NOT NULL DEFAULT 0,  -- Σ cacheable_prompt_tokens（分母）
  turn_count      INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (session_id, cache_scope_id, window_start)
);

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

CREATE TABLE probe_budget_windows (
  scope_kind    TEXT NOT NULL CHECK (scope_kind IN ('global','tenant','user','session','binding')),
  scope_id      TEXT NOT NULL DEFAULT '*',     -- 哨兵，避免可空列进主键
  window_start  TIMESTAMPTZ NOT NULL,          -- 日窗口，date_trunc('day')
  probe_count   INTEGER NOT NULL DEFAULT 0,
  probe_cost    nonneg_usd NOT NULL DEFAULT 0,
  failure_count INTEGER NOT NULL DEFAULT 0,    -- 供「测活失败 ≤ 错误预算 10%」判定
  PRIMARY KEY (scope_kind, scope_id, window_start)
);

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

CREATE TABLE collector_host_rate_limits (
  host            TEXT PRIMARY KEY CHECK (host <> ''),
  next_allowed_at TIMESTAMPTZ NOT NULL,
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE hub_sync_config (
  id               SMALLINT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
  webdav_url       TEXT NOT NULL DEFAULT '',
  webdav_username  TEXT NOT NULL DEFAULT '',
  webdav_password  TEXT NOT NULL DEFAULT '',    -- 明文（FR-113 一期）
  backup_password  TEXT NOT NULL DEFAULT '',    -- all-api-hub 的备份加密密码，明文备份时留空
  enabled          BOOLEAN NOT NULL DEFAULT false,
  interval_minutes INTEGER NOT NULL DEFAULT 360 CHECK (interval_minutes >= 5),
  apply_mode       TEXT NOT NULL DEFAULT 'report'
                     CHECK (apply_mode IN ('report','import')),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE hub_sync_runs (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  started_at   TIMESTAMPTZ NOT NULL,
  finished_at  TIMESTAMPTZ NOT NULL,
  trigger      TEXT NOT NULL CHECK (trigger IN ('schedule','manual')),
  applied      BOOLEAN NOT NULL,               -- 这轮到底落没落库
  error        TEXT NOT NULL DEFAULT '',       -- 空串 = 这轮成功
  result       JSONB
);

CREATE INDEX idx_hub_sync_runs_recent ON hub_sync_runs (started_at DESC, id DESC);

CREATE TABLE collector_credentials (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  account_id      BIGINT NOT NULL REFERENCES upstream_accounts(id),
  channel_id      BIGINT NOT NULL REFERENCES channels(id),
  site_family     TEXT NOT NULL CHECK (site_family IN ('newapi','sub2api','unknown')),
  cred_type       TEXT NOT NULL CHECK (cred_type IN
                    ('newapi_access_token','sub2api_jwt')),
  -- 明文（FR-113）；NewAPI 长期令牌 / Sub2API access+refresh
  access_token    TEXT,
  refresh_token   TEXT,                         -- 仅有 refresh 路径的站型（Sub2API：24h JWT + 无密码续期）
  external_user_id TEXT,                        -- NewAPI New-API-User 头必需
  user_id_header_name TEXT,                     -- 二开 fan-out：New-API-User/Veloera-User/...（§3.1）
  token_expires_at TIMESTAMPTZ,                 -- 有到期时间的站型填（Sub2API 24h）；到期前 RefreshLead 内续期
  -- 凭证互斥作废风险（ISSUE-002 §4）：NewAPI 重生令牌作废旧、Sub2API 并发刷新互斥
  refresh_lock_key TEXT,                        -- 按账号加互斥锁串行刷新
  status          TEXT NOT NULL DEFAULT 'valid'
                    CHECK (status IN ('valid','expiring','invalid','needs_relogin')),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT collector_credentials_refresh_lock_key_check
    CHECK (site_family <> 'sub2api' OR NULLIF(refresh_lock_key, '') IS NOT NULL)
);

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

CREATE VIEW collector_snapshots_v AS
  SELECT *, (valid_until IS NOT NULL AND valid_until < now()) AS is_stale
  FROM collector_snapshots;

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
  -- ↑ 由采集器按站型映射（各家族各自的余量与状态字段）；`unknown` → selector 默认排除（FR-118）。
  --   ⚠️ 该保守默认是我方硬要求：任何上游或第三方组件的宽松默认（"未知即保留"）不得覆盖它。
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_cred_channel     ON collector_credentials(channel_id, status);

CREATE UNIQUE INDEX idx_cred_account_unique ON collector_credentials (account_id);

CREATE INDEX idx_snap_scope       ON collector_snapshots(channel_id, scope_type, fetched_at DESC);

CREATE INDEX idx_snap_stale       ON collector_snapshots(valid_until) WHERE valid_until IS NOT NULL;

CREATE INDEX idx_balsig_account   ON balance_signals(account_id);

CREATE INDEX idx_balsig_state     ON balance_signals(balance_state) WHERE balance_state<>'normal';

CREATE INDEX idx_balsig_quota     ON balance_signals(quota_status) WHERE quota_status='unknown'; -- FR-118 保守排除

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

CREATE UNIQUE INDEX uq_alert_active ON alert_events(dedup_key) WHERE state <> 'closed';

CREATE INDEX idx_alert_open ON alert_events(severity, started_at) WHERE state<>'closed';

CREATE TABLE requests_2026_08 PARTITION OF requests
  FOR VALUES FROM ('2026-08-01') TO ('2026-09-01');

CREATE TABLE attempts_2026_08 PARTITION OF attempts
  FOR VALUES FROM ('2026-08-01') TO ('2026-09-01');

CREATE TABLE attempt_usage_2026_08 PARTITION OF attempt_usage
  FOR VALUES FROM ('2026-08-01') TO ('2026-09-01');

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

CREATE INDEX idx_outbox_undelivered ON ledger_outbox(attempt_id, request_created_at, event_type)
  WHERE delivered_at IS NULL;
