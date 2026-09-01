-- P1 支持同一渠道管理并采集多个账号；凭证归属于账号，不再归属于整个渠道。

ALTER TABLE collector_credentials
  ADD COLUMN account_id BIGINT REFERENCES upstream_accounts(id);

-- 旧库中若某渠道只有凭证而没有账号，先建立对应的账号骨架。
INSERT INTO upstream_accounts (channel_id, external_user_id, status)
SELECT c.channel_id, NULLIF(btrim(c.external_user_id), ''), 'active'
  FROM collector_credentials AS c
 WHERE NOT EXISTS (
   SELECT 1 FROM upstream_accounts AS a
    WHERE a.channel_id = c.channel_id
      AND NULLIF(btrim(c.external_user_id), '') IS NOT NULL
      AND btrim(a.external_user_id) = btrim(c.external_user_id)
 )
   AND NOT EXISTS (
   SELECT 1 FROM upstream_accounts AS a
    WHERE a.channel_id = c.channel_id
 )
   AND NULLIF(btrim(c.external_user_id), '') IS NOT NULL;

-- 优先按上游账号 ID 精确关联；旧数据通常每个渠道只有一个账号，剩余行按唯一账号回填。
UPDATE collector_credentials AS c
   SET account_id = a.id
  FROM upstream_accounts AS a
 WHERE a.channel_id = c.channel_id
   AND NULLIF(btrim(c.external_user_id), '') IS NOT NULL
   AND btrim(a.external_user_id) = btrim(c.external_user_id);

WITH only_account AS (
  SELECT channel_id, min(id) AS account_id
    FROM upstream_accounts
   GROUP BY channel_id
  HAVING count(*) = 1
)
UPDATE collector_credentials AS c
   SET account_id = a.account_id
  FROM only_account AS a
 WHERE c.account_id IS NULL
   AND a.channel_id = c.channel_id;

ALTER TABLE collector_credentials
  ALTER COLUMN account_id SET NOT NULL;

DROP INDEX idx_cred_channel_unique;
CREATE UNIQUE INDEX idx_cred_account_unique
  ON collector_credentials (account_id);
