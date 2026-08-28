-- 健康/冷却/容量/canary/probe 占用（02 §6）
-- 由 verify/split_migrations.py 从 docs/dev/02-data-model.md 生成初始基线。
-- ⚠️ 此后本文件**不可修改**（已应用的迁移不可变）；schema 变更请新增迁移文件。

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
