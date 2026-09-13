-- all-api-hub 的 WebDAV 定时同步配置（单行）。
--
-- 为什么不进 config_params：那张表会把每次变更的旧值留在 prev_value 里，
-- 而 /admin/config 的列表接口会把值原样回显 —— 两条加起来等于把 WebDAV 密码
-- 和备份解密密码摊在审计历史与接口响应里。凭证类的东西在本仓库一律
-- "明文存库、只报存在性、不回显内容"（collector_credentials 同此）。
--
-- 单行：同步的是"本网关 ↔ 一个 all-api-hub 备份"，没有第二份。用 id=1 的
-- CHECK 钉死，比在应用层约定"只读第一行"要可靠 —— 后者迟早会插进第二行。

CREATE TABLE hub_sync_config (
  id               SMALLINT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
  webdav_url       TEXT NOT NULL DEFAULT '',
  webdav_username  TEXT NOT NULL DEFAULT '',
  -- 明文（与 collector_credentials 同一取舍，FR-113 一期）
  webdav_password  TEXT NOT NULL DEFAULT '',
  -- all-api-hub 的"备份加密密码"。远端是明文备份时留空。
  backup_password  TEXT NOT NULL DEFAULT '',
  enabled          BOOLEAN NOT NULL DEFAULT false,
  -- 下限 5 分钟：这是别人家的 WebDAV，且每轮要探测上百个站点。
  interval_minutes INTEGER NOT NULL DEFAULT 360 CHECK (interval_minutes >= 5),
  -- report = 只拉取比对、不落库；import = 等同界面上的"正式导入"。
  apply_mode       TEXT NOT NULL DEFAULT 'report'
                     CHECK (apply_mode IN ('report','import')),
  last_run_at      TIMESTAMPTZ,
  -- 空串 = 上一轮成功。"从未跑过"看 last_run_at IS NULL，不另设状态列。
  last_error       TEXT NOT NULL DEFAULT '',
  last_result      JSONB,
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO hub_sync_config (id) VALUES (1) ON CONFLICT (id) DO NOTHING;
