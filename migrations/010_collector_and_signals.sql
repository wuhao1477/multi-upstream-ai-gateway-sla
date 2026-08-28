-- 采集凭证/快照/余额信号（02 §7）
-- 由 verify/split_migrations.py 从 docs/dev/02-data-model.md 生成初始基线。
-- ⚠️ 此后本文件**不可修改**（已应用的迁移不可变）；schema 变更请新增迁移文件。

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
  -- ↑ 由采集器按站型映射（NewAPI/Sub2API/ASXS 各自的余量与状态字段）；`unknown` → selector 默认排除（FR-118）。
  --   ⚠️ 该保守默认是我方硬要求：任何上游或第三方组件的宽松默认（"未知即保留"）不得覆盖它。
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_cred_channel     ON collector_credentials(channel_id, status);

CREATE INDEX idx_snap_scope       ON collector_snapshots(channel_id, scope_type, fetched_at DESC);

CREATE INDEX idx_snap_stale       ON collector_snapshots(valid_until) WHERE valid_until IS NOT NULL;

CREATE INDEX idx_balsig_account   ON balance_signals(account_id);

CREATE INDEX idx_balsig_state     ON balance_signals(balance_state) WHERE balance_state<>'normal';

CREATE INDEX idx_balsig_quota     ON balance_signals(quota_status) WHERE quota_status='unknown'; -- FR-118 保守排除
