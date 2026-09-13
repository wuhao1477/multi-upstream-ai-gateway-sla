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
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO hub_sync_config (id) VALUES (1) ON CONFLICT (id) DO NOTHING;

-- 每一轮同步的记录。
--
-- ⚠️ 配置表上**不留** last_run_at / last_error / last_result：那三列与本表是
-- 同一件事的两份存放，迟早分叉（一处更新了另一处没有，而界面读的是没更新的
-- 那份）。"上次同步"一律由本表最新一行推导，`hub_sync_due` 的判据也读它。
--
-- result 存**整份**导入结果，含逐站明细 —— 打开一条历史要回答的正是"哪个站点
-- 失败了、为什么"，只留计数的话这张表就只能告诉你"失败了 3 个"。
-- 代价用保留条数兜住，见下面的 hub_sync_runs_prune 说明。
CREATE TABLE hub_sync_runs (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  started_at   TIMESTAMPTZ NOT NULL,
  finished_at  TIMESTAMPTZ NOT NULL,
  -- schedule = 定时器自己醒来跑的；manual = 人在界面上点的。
  -- 分开记是因为排查时的第一个问题就是"这轮是谁触发的"。
  trigger      TEXT NOT NULL CHECK (trigger IN ('schedule','manual')),
  -- 这一轮到底落没落库。report 模式下的数字不代表建出了渠道，
  -- 不记它的话历史列表里两种轮次看起来一模一样。
  applied      BOOLEAN NOT NULL,
  -- 空串 = 这轮成功。
  error        TEXT NOT NULL DEFAULT '',
  result       JSONB
);

-- 按时间倒序翻页用。列表只读最近几十条，走索引即可。
CREATE INDEX idx_hub_sync_runs_recent ON hub_sync_runs (started_at DESC, id DESC);
