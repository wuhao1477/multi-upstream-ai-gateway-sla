-- 网关调用方凭证与配额（02 §2bis，P2 才写入）
-- 由 verify/split_migrations.py 从 docs/dev/02-data-model.md 生成初始基线。
-- ⚠️ 此后本文件**不可修改**（已应用的迁移不可变）；schema 变更请新增迁移文件。

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
