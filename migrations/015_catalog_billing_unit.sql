-- 015：channel_model_catalog 补 billing_unit（第 46 轮真实数据发现）
--
-- 为什么必须有：NewAPI 新版有**两种计价形态**，实测 1369 个模型里 208 个
-- （15%）是按次固定价：
--   quota_type=0 → model_ratio 是相对基准价的**倍率**，口径 per_1m_token
--   quota_type=1 → model_price 是每次调用的**绝对美元价**，口径 per_call
--
-- ⚠️ 两者的数值区间**重叠**（实测按次价 0.08~0.56，倍率 0.685~30），
-- 因此**无法从数值反推口径**。没有这一列，目录里的 input_price 就是个
-- 无单位的数字：把 $0.15/次 当成倍率 0.15 参与成本排序，会让按次计价的
-- 模型看起来比实际便宜若干个数量级，而这类模型往往正是最贵的（如 GPT-4 图像）。
--
-- 这正是 02 §3 与 #7 反复强调的 billing_unit 缩放风险，只不过它先在 P1
-- 的目录表上现形，而非等到 P3 的成本计算。
ALTER TABLE channel_model_catalog
  ADD COLUMN IF NOT EXISTS billing_unit TEXT
    CHECK (billing_unit IN ('per_1m_token','per_1k_token','per_token','per_call'));

COMMENT ON COLUMN channel_model_catalog.billing_unit IS
  '计价口径；per_call 时 input_price/output_price 是每次调用的绝对美元价，'
  '其余为相对基准价的倍率。缺失表示上游未声明，消费方须按未知处理而非默认 token 计价。';
