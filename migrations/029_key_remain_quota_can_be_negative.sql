-- 029：upstream_keys.remain_quota_usd 允许为负
--
-- **这是一个真实的采集失败，不是理论洁癖。** 2026-09-15 实测两个真站点，
-- 上游 /api/token 回的 `remain_quota` 有负数：
--
--   id=5574  remain_quota=-3017798  used_quota=3517798  unlimited_quota=true
--   id=5158  remain_quota=-22       used_quota=22       unlimited_quota=true
--
-- NewAPI 对不限额 Key 是"从一个名义额度往下扣"，扣穿了就是负数（欠费/超支）。
-- 而 002 把这一列建成了 `nonneg_usd`（CHECK VALUE >= 0），于是 UpdateKeyUsage
-- 在这两把 Key 上必然失败：
--
--   更新 Key 4 用量: ERROR: value for domain nonneg_usd violates check constraint
--
-- 后果不是一把 Key 的事 —— 那一轮该渠道的 `keys` 能力整项报 failed，
-- 该渠道所有 Key 的配额从此不再更新。
--
-- 这个约束从 002 起就在，只是一直没暴露：此前库里的 Key 都是人工登记的
-- 测试 Key（余额 0 或正数），2026-09-15 第一次导入带真实用量的上游 Key 才撞上。
--
-- ⚠️ **只放开 remain，不放开 used**：
--   · remain 为负是**有意义的事实**（欠了多少），要如实存下来；
--   · used 为负没有任何真实含义，它仍该被拦下。
-- 所以这里换成裸 NUMERIC 而不是把 nonneg_usd 这个域本身改宽 —— 域是共享的，
-- 改它会一并放开 gateway_clients 的预留/结算金额与 price_versions 的价格，
-- 那些地方负值确实是错误。
--
-- 精度与 nonneg_usd 一致（NUMERIC(20,10)），只去掉那条 CHECK。

ALTER TABLE upstream_keys
  ALTER COLUMN remain_quota_usd TYPE NUMERIC(20,10);

COMMENT ON COLUMN upstream_keys.remain_quota_usd IS
  '这把 Key 还被允许花多少（归一美元）。**可以为负**：不限额 Key 扣穿名义额度后上游就回负数（2026-09-15 实测）。负数 = 已超支，不是脏数据。';
