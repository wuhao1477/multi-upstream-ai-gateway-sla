-- 035：非 Cookie 凭证加密存储（D1，#34）第二步 —— 删除明文列。
--
-- ⚠️ 执行本文件之前，迁移器在**同一事务**里先运行 Go 回填
-- （store.encryptPlaintextCredentials）：把现有明文加密写入 034 的密文列，并逐行
-- 解密比对。库里存在明文而未配置 SLA_CREDENTIAL_SECRET_KEY、或任何一行比对
-- 不一致时，整个事务回滚 —— 明文列保留，本文件不记为已应用，进程拒绝启动。
-- 因此不会出现"部分加密、部分明文"的库，也不需要长期维护明文兼容读取。
--
-- 执行前提（维护窗口、停止全部 core/collector、备份及对应密钥）见
-- docs/dev/06-deployment-and-operations.md。

ALTER TABLE upstream_keys
  DROP COLUMN secret,
  ALTER COLUMN secret_ciphertext SET NOT NULL,
  ALTER COLUMN secret_prefix SET NOT NULL;

ALTER TABLE collector_credentials
  DROP COLUMN access_token,
  DROP COLUMN refresh_token;

ALTER TABLE hub_sync_config
  DROP COLUMN webdav_url,
  DROP COLUMN webdav_password,
  DROP COLUMN backup_password;
