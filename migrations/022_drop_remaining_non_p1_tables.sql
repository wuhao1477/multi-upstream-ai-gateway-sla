-- P1 只保留上游采集与管理运行面。
-- 已应用迁移不可改，因此用前向迁移删除先前提前创建的 P2/P3 对象。

-- 先删子表，再删它们引用的注册表；不用 CASCADE，
-- 便于在仍有保留对象依赖时直接失败。
DROP TABLE IF EXISTS ledger_outbox;
DROP TABLE IF EXISTS attempt_usage;
DROP TABLE IF EXISTS attempts;
DROP TABLE IF EXISTS requests;
DROP TABLE IF EXISTS session_prefix_ledger;
DROP TABLE IF EXISTS probe_templates;
DROP TABLE IF EXISTS multiplier_versions;
DROP TABLE IF EXISTS bindings;
DROP TABLE IF EXISTS cache_scopes;
DROP TABLE IF EXISTS model_aliases;
DROP TABLE IF EXISTS routing_policies;
DROP TABLE IF EXISTS gateway_clients;

-- 未实现的账密凭证通路不属于 P1。
ALTER TABLE collector_credentials
  DROP CONSTRAINT IF EXISTS collector_credentials_cred_type_check;
ALTER TABLE collector_credentials
  ADD CONSTRAINT collector_credentials_cred_type_check
  CHECK (cred_type IN ('newapi_access_token', 'sub2api_jwt'));
ALTER TABLE collector_credentials
  DROP COLUMN IF EXISTS username,
  DROP COLUMN IF EXISTS password;

-- 保留七个数据库来源的 P1 配置键。admin_token 只来自环境变量。
DELETE FROM config_params
 WHERE param_key NOT IN (
   'collector_request_interval_ms',
   'collector_price_interval_h',
   'collector_balance_interval_min',
   'collector_keyquota_interval_min',
   'collector_catalog_interval_h',
   'catalog_missing_rounds',
   'sync_min_interval_s'
 );
