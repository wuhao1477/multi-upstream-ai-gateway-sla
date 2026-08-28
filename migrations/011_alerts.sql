-- 告警事件（02 §8）
-- 由 verify/split_migrations.py 从 docs/dev/02-data-model.md 生成初始基线。
-- ⚠️ 此后本文件**不可修改**（已应用的迁移不可变）；schema 变更请新增迁移文件。

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
