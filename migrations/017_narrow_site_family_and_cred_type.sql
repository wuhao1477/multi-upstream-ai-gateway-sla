-- 收窄 site_family 与 cred_type 的取值：删掉已移出支持范围的那个站型。
--
-- 决定（2026-08-29）：某闭源自建站不再纳管 —— 运营导出的 65 个站点里
-- Detect 分桶是 52 newapi + 13 sub2api，**一个都不是该站型**，也没有接入需求。
-- 适配器（asxs.go）、注册（register_asxs.go）、Family 常量已随本轮一起删除，
-- 库侧的取值范围要跟着收，否则约束比代码宽 —— 那意味着**代码里已经不存在的
-- 家族仍能写进库**，而写进去之后 Lookup 拿不到注册，采集侧报"无对应适配器"，
-- 停在一个只能靠人去 UPDATE 才能救的状态。
--
-- ⚠️ 为什么是新增 017，而不是回去改 001/010：
--    migrate.go 的 checksum 契约 —— 已应用的迁移文件 checksum 不一致时直接
--    return error 且**不执行任何迁移**。真库已应用 16 个文件，改基线会让整栈
--    起不来。同 016 头部。
--
-- 真库实测（2026-08-29，SLA_DB @ <internal-db-host>，收窄前跑的）：
--    channels:              newapi 52 / sub2api 13
--    collector_credentials: newapi|newapi_access_token 52 / sub2api|sub2api_jwt 13
--    → **0 行会被新约束拦**。所以这里不需要先 UPDATE 数据，也不需要 NOT VALID。
--    （若日后在别的库上跑本迁移而它有存量 asxs 行，ALTER 会直接失败并指出
--     是哪条约束 —— 那是对的：静默改掉别人的数据比报错更糟。）
--
-- **account_password 刻意保留**，不在本次收窄之列：它是"无令牌端点、只能账密
-- 重登"那条通路的库侧一端（04 §5.3）。当前没有站型声明 PasswdCredType，
-- 但那条通路的另外三处（Registration.PasswdCredType、Credential.Username/Password、
-- CredTypeFor 的 hasPasswd 分支）都留着 —— 四处一起留才是一条通路，
-- 删掉这一处会让接自研站时又要多一条迁移，而那条迁移是纯粹的返工。
--
-- 三条约束都用 DROP + ADD 而不是 ALTER ... VALIDATE：CHECK 约束不能原地改定义。
-- 约束名是 PG 自动生成的那个（<表>_<列>_check），已 pg_constraint 核对。

-- ── channels.site_family ──
ALTER TABLE channels DROP CONSTRAINT IF EXISTS channels_site_family_check;
ALTER TABLE channels ADD CONSTRAINT channels_site_family_check
  CHECK (site_family IN ('newapi', 'sub2api', 'unknown'));

-- ── collector_credentials.site_family ──
ALTER TABLE collector_credentials
  DROP CONSTRAINT IF EXISTS collector_credentials_site_family_check;
ALTER TABLE collector_credentials ADD CONSTRAINT collector_credentials_site_family_check
  CHECK (site_family IN ('newapi', 'sub2api', 'unknown'));

-- ── collector_credentials.cred_type ──
-- 取值 = 各 Registration 的 CredType 与 PasswdCredType（04 §7bis）。
-- 加一族要配一条迁移放宽这里，否则新族的凭证**登记时才报约束冲突**。
ALTER TABLE collector_credentials
  DROP CONSTRAINT IF EXISTS collector_credentials_cred_type_check;
ALTER TABLE collector_credentials ADD CONSTRAINT collector_credentials_cred_type_check
  CHECK (cred_type IN ('newapi_access_token', 'sub2api_jwt', 'account_password'));
