-- 026：channel_model_catalog 补 vendor_name 与 endpoint_types
--
-- 两列都来自 NewAPI 的 /api/pricing，2026-09-14 实测一个真实 NewAPI 站点
-- （站点地址不入库，见 verify/test-public-safety.sh）：
--   · 顶层 `vendors` 是数组 [{id,name,icon}]（实测 35 个），每个模型带 `vendor_id`
--     指向它。存**名字**而不是 id：id 是站点自己的自增主键，跨站点毫无意义，
--     而"哪些渠道有 Anthropic 的模型"这个问题要跨渠道聚合，只能按名字聚。
--     同一家公司在不同站点可能叫不同名字（实测同一站里 "Alibaba" 与 "阿里巴巴"
--     同时存在）——**不做归一**，那是站点自己的声明，我方替它改名就是编数据。
--   · 每个模型带 `supported_endpoint_types`（实测取值 openai / anthropic /
--     gemini / openai-response / openai-video / image-generation / jina-rerank），
--     顶层 `supported_endpoint` 给出每个类型对应的 path+method。
--
-- 两列都可空：缺席 = 该站型/该版本不提供这个字段，**不是**"没有供应商"或
-- "不支持任何端点"。消费方按未知渲染，不要补默认值（同 billing_unit 的口径，
-- 015 迁移 + 02 §1.3bis）。
--
-- 不建索引：目录全表约 9 万行，按这两列过滤是毫秒级顺序扫，而它们的基数低
-- （35 个供应商 / 7 种端点），B-tree 选择性差。等它真慢了再说。

ALTER TABLE channel_model_catalog
  ADD COLUMN vendor_name    TEXT,
  ADD COLUMN endpoint_types TEXT[];

COMMENT ON COLUMN channel_model_catalog.vendor_name IS
  '模型发行方（上游 vendors[].name，按 vendor_id 解析）。NULL = 上游未声明，不是"无供应商"。跨站点不归一：那是站点自己的叫法。';
COMMENT ON COLUMN channel_model_catalog.endpoint_types IS
  '上游声明该模型支持的端点类型（supported_endpoint_types，如 openai/anthropic/gemini/image-generation）。NULL = 未声明，不是"不支持任何端点"。';
