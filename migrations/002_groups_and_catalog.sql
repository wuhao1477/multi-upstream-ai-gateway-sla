-- 上游分组与模型目录（02 §1.3，交付阶段 P1）
-- 由 verify/split_migrations.py 从 docs/dev/02-data-model.md 生成初始基线。
-- ⚠️ 此后本文件**不可修改**（已应用的迁移不可变）；schema 变更请新增迁移文件。

CREATE TABLE channel_groups (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  channel_id    BIGINT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  group_ref     TEXT NOT NULL,               -- 上游分组标识（NewAPI group / Sub2API group_id）
  rate_multiplier NUMERIC(12,6),             -- 分组倍率（采集所得；历史版本仍走 multiplier_versions）
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
  first_seen_at TIMESTAMPTZ NOT NULL,
  last_seen_at  TIMESTAMPTZ NOT NULL,        -- 停止更新 = 上游下架了它（FR-126 告警判据）
  PRIMARY KEY (channel_id, model_name)
);

ALTER TABLE upstream_keys
  ADD COLUMN channel_group_id BIGINT REFERENCES channel_groups(id),  -- Key 归属分组（FR-123）
  ADD COLUMN remain_quota_usd nonneg_usd,     -- 剩余额度（归一美元，FR-125）
  ADD COLUMN used_quota_usd   nonneg_usd,     -- 已用额度（FR-125）
  ADD COLUMN rpm_limit        INTEGER,        -- 上游 Key 级 RPM（FR-127；**P1 只存不判**）
  ADD COLUMN concurrency_limit INTEGER,       -- 上游 Key 级并发（FR-127；同上）
  ADD COLUMN quota_synced_at  TIMESTAMPTZ;    -- 最近同步时刻（陈旧判定，FR-128）

CREATE INDEX idx_chgroups_channel ON channel_groups(channel_id);

CREATE INDEX idx_catalog_channel  ON channel_model_catalog(channel_id, last_seen_at DESC);

CREATE INDEX idx_keys_group       ON upstream_keys(channel_group_id);
