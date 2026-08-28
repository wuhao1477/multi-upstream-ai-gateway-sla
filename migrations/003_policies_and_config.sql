-- 别名与策略域 + config_params（02 §2）
-- 由 verify/split_migrations.py 从 docs/dev/02-data-model.md 生成初始基线。
-- ⚠️ 此后本文件**不可修改**（已应用的迁移不可变）；schema 变更请新增迁移文件。

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
