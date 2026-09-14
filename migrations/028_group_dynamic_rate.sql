-- 028：channel_groups 补 rate_dynamic 与 dynamic_candidates
--
-- 有一类分组**没有自己的固定倍率**：NewAPI 的 `auto`。2026-09-14 复核
-- QuantumNous/new-api 源码（middleware/auth.go → middleware/distributor.go →
-- service/group.go）：token.Group == "auto" 时进入自动模式，实际计费用
-- `auto_groups` 候选里**第一个有可用渠道**的那个分组的倍率；源码显式排除了
-- "auto" 自己（`if groupName == "" || groupName == "auto" { return false }`）。
--
-- 而 /api/pricing 的 group_ratio 里 "auto" 照样带着一个倍率（实测某站是 1）。
-- 那个 1 是展示用的占位，不是计费倍率。只存它的话，界面会报一个与账单无关、
-- 却看起来完全正常的数字 —— 实测某站三个候选组的倍率是 0.26 / 1 / 2.6，
-- 差十倍。
--
-- 所以把这类分组的倍率存成**动态**：
--   · rate_dynamic = true          → 这一组的倍率不是固定值，不可拿来算钱
--   · dynamic_candidates = {…}     → 候选分组名，倍率去它们各自的行里取
-- rate_multiplier 照旧保留上游给的那个数（它是事实，只是不能当计费倍率用），
-- 消费方必须先看 rate_dynamic。
--
-- ⚠️ **两列都不表示"未采到"**。rate_dynamic=false + rate_multiplier IS NULL
-- 才是未采到。三种状态要分得开：固定倍率 / 动态倍率 / 不知道。
--
-- ⚠️ **空分组的 Key 与动态分组是两回事。** Key 的 group 为空串时走的是账号的
-- 默认分组（027 的 account_group），那通常是一个有固定倍率的普通分组 ——
-- 只有当账号自己的分组恰好是 auto 时才落到动态这一档。不可把"没写分组"
-- 一概当成动态。
--
-- 候选存**名字**不存外键：候选列表由上游按请求者过滤后给出，其中可能有我方
-- 尚未采到的分组。存名字则"上游说候选里有 X、我方没有 X 的倍率"这个事实还在；
-- 存外键只能把它丢掉。

ALTER TABLE channel_groups
  ADD COLUMN rate_dynamic       BOOLEAN NOT NULL DEFAULT false,
  ADD COLUMN dynamic_candidates TEXT[];

COMMENT ON COLUMN channel_groups.rate_dynamic IS
  '该分组的倍率不是固定值（如 NewAPI 的 auto：运行时在 dynamic_candidates 里选一个组计费）。为真时 rate_multiplier 不可拿来算钱。';
COMMENT ON COLUMN channel_groups.dynamic_candidates IS
  '动态倍率的候选分组名（NewAPI 的 auto_groups）。存名字不存外键：候选里可能有我方尚未采到的分组。';
