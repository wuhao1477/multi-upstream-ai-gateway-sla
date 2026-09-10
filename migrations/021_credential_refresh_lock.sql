-- 为存量凭证补齐跨实例刷新锁键（04 §5.2，不变式 S-1）。
-- external_user_id 是登记时已有的账号身份；没有它的旧行退回渠道级键，
-- 只会少共享，不会把不同账号错误合并。
-- channels.base_url 的写入入口已统一存规范形态：去空白、小写 scheme/host、保留
-- path 大小写、去尾斜杠。这里不再 lower 整串 URL，避免把 /API 与 /api 合并。
WITH source AS (
  SELECT c.channel_id, c.site_family,
         COALESCE(NULLIF(btrim(c.external_user_id), ''), 'channel:' || c.channel_id::text) AS account_id,
         rtrim(btrim(ch.base_url), '/') AS base_url
    FROM collector_credentials AS c
    JOIN channels AS ch ON ch.id = c.channel_id
   WHERE c.site_family = 'sub2api'
     AND NULLIF(c.refresh_lock_key, '') IS NULL
), parts AS (
  SELECT channel_id, site_family, account_id,
         lower(substring(base_url FROM '^([^:/?#]+)')) AS scheme,
         substring(base_url FROM '^[^:/?#]+://([^/?#]*)') AS authority,
         COALESCE(substring(base_url FROM '^[^:/?#]+://[^/?#]*(.*)$'), '') AS suffix
    FROM source
)
UPDATE collector_credentials AS c
   SET refresh_lock_key = 'refresh:' || p.site_family || ':' ||
       p.scheme || '://' ||
       CASE WHEN position('@' IN p.authority) > 0
            THEN substring(p.authority FROM '^(.+@)') ||
                 lower(substring(p.authority FROM '^.+@(.*)$'))
            ELSE lower(p.authority)
       END || p.suffix || ':' || p.account_id
  FROM parts AS p
 WHERE c.channel_id = p.channel_id;

ALTER TABLE collector_credentials
  ADD CONSTRAINT collector_credentials_refresh_lock_key_check
  CHECK (site_family <> 'sub2api' OR NULLIF(refresh_lock_key, '') IS NOT NULL);
