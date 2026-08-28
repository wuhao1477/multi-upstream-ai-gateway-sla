-- 月分区与账本 outbox（02 §9）
-- 由 verify/split_migrations.py 从 docs/dev/02-data-model.md 生成初始基线。
-- ⚠️ 此后本文件**不可修改**（已应用的迁移不可变）；schema 变更请新增迁移文件。

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
