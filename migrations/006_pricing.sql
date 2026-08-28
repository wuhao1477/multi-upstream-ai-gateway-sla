-- 价格版本域（02 §3）
-- 由 verify/split_migrations.py 从 docs/dev/02-data-model.md 生成初始基线。
-- ⚠️ 此后本文件**不可修改**（已应用的迁移不可变）；schema 变更请新增迁移文件。

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
