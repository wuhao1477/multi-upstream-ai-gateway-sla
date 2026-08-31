-- 018：price_versions.billing_unit 去掉默认值、补 CHECK、改可空
--
-- 发版复评（2026-08-29）查出的**两张表处置相反**，而不设防的那张恰是权威表：
--
--   | 表                    | 可空 | 默认值          | CHECK | 谁读它       |
--   | channel_model_catalog | YES  | 无              | 有    | P1 界面展示  |
--   | price_versions        | NO   | 'per_1m_token'  | 无    | P3 成本计算  |
--
-- 015 给目录补了「可空 + CHECK」，02 §1.3bis 随之定下规则：**无价则口径留
-- NULL，不补默认值** —— 补 per_1m_token 会把"上游未声明"伪装成"已知按 token
-- 计价"。但 006（早于 015）已把权威表定成 NOT NULL DEFAULT，于是同一条写入
-- 路径（internal/store/sink.go 的 SavePricing）只能写
-- `COALESCE(NULLIF($7,''),'per_1m_token')` —— **015 想禁止的事，006 的列定义
-- 强迫它做**。一个按次计价却没声明口径的模型，在目录里是 NULL，在权威表里是
-- per_1m_token；而 02 §3 的成本公式正是 `/ unit_tokens(pv.billing_unit)`，
-- per_1m_token 对应除以 1,000,000。
--
-- 没 CHECK 是另一半：口径字符串拼错（'per_1M_token'、'per1m'）照收，而
-- unit_tokens 查表查不到那个值 —— 要么 panic，要么落到某个默认分支，
-- 两种都比写入时就被拦下来差。目录表有 CHECK，权威表没有。
--
-- **当前爆炸半径为零，但不是因为它被防住了**：SavePricing 只为**已登记为可路由
-- 模型**（models 表）的条目写价格版本，而 models 按 AC-39 恒为 0 行（P1 无路由，
-- "启用模型"这个动作不存在）。实测真库 price_versions **0 行** /
-- channel_model_catalog 2905 行 —— 那句 COALESCE 一次都没执行过。
-- 所以这是条**触发条件已知的定时缺陷**：P3 登记可路由模型的那一刻开始写。
-- 趁 0 行改，不需要数据迁移，也不需要 NOT VALID。
--
-- ⚠️ 为什么改**可空**而不是「保持 NOT NULL + 加 CHECK」：
--    后者更强（价格必有口径，一条无口径的价格根本进不来），也确实是当前唯一可能的
--    形态（现役两个站型里，Sub2API 的 FetchPricing 不返回 Models，SavePricing 直接
--    早退；NewAPI 的 BillingUnit() 只会返回 per_call 或 per_1m_token，不会空）。
--    但两张 billing_unit 列**列定义一致**这件事本身有价值：02 §1.3bis 的
--    「缺失表示上游未声明，消费方须按未知处理而非默认 token 计价」是一条**跨表**
--    规则，一张表用 NULL 表达未声明、另一张用"写不进来"表达，读 schema 的人就得
--    先判断这一列在哪张表上语义是什么 —— 而两表定义不一致正是本缺陷的成因。
--    NOT NULL 还有一处实际代价：日后接入一个"有价但不声明口径"的站型时，
--    整个渠道的 SavePricing 会在第一条上整批失败，而正确的降级是**落 NULL 并让
--    消费方按未知处理**。
--
-- ⚠️ 为什么是新增 018，而不是回去改 006：
--    migrate.go 的 checksum 契约 —— 已应用的迁移文件 checksum 不一致时直接
--    return error 且**不执行任何迁移**。真库已应用 17 个文件。同 016/017 头部。

ALTER TABLE price_versions ALTER COLUMN billing_unit DROP DEFAULT;
ALTER TABLE price_versions ALTER COLUMN billing_unit DROP NOT NULL;

-- 取值与 channel_model_catalog_billing_unit_check **逐字相同**。
-- test-migrate.sh 7/7 断言两处 CHECK 的取值集合相等 —— 于是日后加一种口径
-- （如 per_1k_char）漏改一处会红，而不是让权威表比目录表宽。
ALTER TABLE price_versions
  ADD CONSTRAINT price_versions_billing_unit_check
    CHECK (billing_unit IN ('per_1m_token','per_1k_token','per_token','per_call'));

COMMENT ON COLUMN price_versions.billing_unit IS
  '计价口径；成本公式必须按它缩放（02 §3：漏缩放放大 1,000,000 倍）。'
  'per_call 时 input_price/output_price 是每次调用的绝对美元价，与 token 数无关。'
  '缺失表示上游未声明，消费方须按未知处理而非默认 token 计价 —— 不得补默认值。';
