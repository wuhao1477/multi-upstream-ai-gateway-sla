-- 034：非 Cookie 凭证加密存储（D1，#34）第一步 —— 只加密文列。
--
-- 本文件只加列、不动数据：失败或回退旧版本都无害（旧版本不读这些列）。
-- 加密回填与删除明文列在 035 的同一个事务里完成；回填需要部署密钥，
-- 只能由 Go 执行（store.encryptPlaintextCredentials），纯 SQL 做不了。
--
-- 密文格式与 AAD（表名、记录 ID、字段名）见 internal/store/credential_cipher.go。

ALTER TABLE upstream_keys
  ADD COLUMN secret_ciphertext BYTEA,
  -- 明文前 8 个字符，供列表定位。由 Go 在写入秘密时同事务计算，
  -- 列表只读它，不为展示解密整份 Key 列表。
  ADD COLUMN secret_prefix     TEXT;

ALTER TABLE collector_credentials
  ADD COLUMN access_token_ciphertext  BYTEA,
  ADD COLUMN refresh_token_ciphertext BYTEA;

ALTER TABLE hub_sync_config
  -- WebDAV 地址整体加密：query 参数、userinfo 都可能承载认证信息。
  ADD COLUMN webdav_url_ciphertext      BYTEA,
  -- 去掉 userinfo/query/fragment 后的展示值，管理接口只回它。
  ADD COLUMN webdav_url_display         TEXT NOT NULL DEFAULT '',
  ADD COLUMN webdav_password_ciphertext BYTEA,
  ADD COLUMN backup_password_ciphertext BYTEA;
