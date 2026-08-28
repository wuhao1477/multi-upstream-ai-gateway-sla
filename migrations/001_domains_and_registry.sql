-- 金额域 + 资源注册域（02 §0.2/§1.1/§1.2）
-- 由 verify/split_migrations.py 从 docs/dev/02-data-model.md 生成初始基线。
-- ⚠️ 此后本文件**不可修改**（已应用的迁移不可变）；schema 变更请新增迁移文件。

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
  site_family     TEXT NOT NULL CHECK (site_family IN ('newapi','sub2api','asxs','unknown')), -- ISSUE-002 §1 三家族
  base_url        TEXT NOT NULL,
  upstream_provider_id BIGINT REFERENCES upstream_providers(id),   -- 真实上游（故障域根，FR-044）
  status          TEXT NOT NULL DEFAULT 'enabled' CHECK (status IN ('enabled','disabled')), -- FR-004
  disabled_reason TEXT,           -- FR-095 人工停用原因
  disabled_until  TIMESTAMPTZ,    -- FR-095 有效期
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

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
