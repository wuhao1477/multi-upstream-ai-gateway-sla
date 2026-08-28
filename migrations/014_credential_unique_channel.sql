-- collector_credentials 一渠道一份（P1 假设）
--
-- 为何需要：凭证续期是 upsert 语义（refresh 后写回新令牌，04 §5），
-- 没有唯一约束就无法 ON CONFLICT，只能"先查再插/改" —— 那是 check-then-act，
-- 而凭证刷新恰恰是并发敏感的（不变式 S-1）。
--
-- 为何是 channel_id 而非 (channel_id, cred_type)：一个渠道只有一套采集凭证；
-- 多账号分别采集属后续阶段，届时按 (channel_id, external_user_id) 扩展
-- （加列 + 换约束，届时再出一个迁移）。

CREATE UNIQUE INDEX idx_cred_channel_unique
  ON collector_credentials (channel_id);
