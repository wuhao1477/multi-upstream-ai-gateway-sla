-- 数据许可域（02 §2ter，默认关闭）
-- 由 verify/split_migrations.py 从 docs/dev/02-data-model.md 生成初始基线。
-- ⚠️ 此后本文件**不可修改**（已应用的迁移不可变）；schema 变更请新增迁移文件。

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
