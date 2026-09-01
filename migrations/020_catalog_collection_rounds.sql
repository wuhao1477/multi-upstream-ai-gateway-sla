-- 020：模型目录按真实成功采集轮次判定疑似下架。
-- 时间差不能代表连续轮次：任务延迟、手动刷新和配置改周期都会误判。

ALTER TABLE channels
  ADD COLUMN catalog_sync_seq BIGINT NOT NULL DEFAULT 0,
  ADD CONSTRAINT channels_catalog_sync_seq_nonnegative CHECK (catalog_sync_seq >= 0);

ALTER TABLE channel_model_catalog
  ADD COLUMN last_seen_seq BIGINT NOT NULL DEFAULT 0,
  ADD CONSTRAINT channel_model_catalog_last_seen_seq_nonnegative CHECK (last_seen_seq >= 0);

COMMENT ON COLUMN channels.catalog_sync_seq IS
  '该渠道可靠模型目录采集的成功轮次；仅在模型存在性信息完整时递增。';

COMMENT ON COLUMN channel_model_catalog.last_seen_seq IS
  '该模型最近出现的 catalog_sync_seq；二者差值达到 catalog_missing_rounds 时视为疑似下架。';
