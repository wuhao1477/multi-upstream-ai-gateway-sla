-- upstream_keys.external_ref：上游侧的 Key 标识（FR-125 用量匹配所需）
--
-- 为何需要它：采集只能拿到上游的 key id/name（脱敏引用），拿不到明文 secret，
-- 因此无法用 secret 反查库中是哪一行。没有这一列，"把采到的用量写回对应 Key"
-- 就无从落地 —— 而那正是 FR-125 的核心。
--
-- 为何是新文件而不改 001：已应用的迁移不可修改（改了会让已部署环境与新环境
-- schema 不一致，internal/store/migrate.go 有 checksum 守卫）。
--
-- 唯一性作用域是账号而非渠道：同一渠道下不同账号可能在上游各有一把 id 相同的
-- Key（各自的 id 空间独立）。

ALTER TABLE upstream_keys
  ADD COLUMN external_ref TEXT;

CREATE UNIQUE INDEX idx_keys_external_ref
  ON upstream_keys (account_id, external_ref)
  WHERE external_ref IS NOT NULL;
