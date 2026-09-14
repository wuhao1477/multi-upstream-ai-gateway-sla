-- 027：upstream_accounts 补 account_group（账号在上游的默认分组）
--
-- 为什么需要它：Key 的分组可以是**空的**，而空不等于"没有分组"。
-- 2026-09-14 实测两个真站点：
--   · 上游 /api/token 的 `group` 为空串时，调用走的是**这个账号自己的分组**
--     （/api/user/self 的 `group`，两站实测都是 "default"）；
--   · 于是一把 group="" 的 Key 并非"未归组"，而是"跟账号走"。
-- 不采这一列的话，这种 Key 在界面上永远显示未归组、倍率未知，而它其实是
-- 有明确倍率的（站 A 的 default 是 ×1）。
--
-- 存**名字**而不是 channel_groups 的外键：账号的分组由上游随时可改，而
-- 我方采到的 channel_groups 只覆盖 group_ratio 里列出的那些。实测站 B 的
-- 账号分组是 "default" 但它的 group_ratio 里**没有** default —— 存外键就
-- 只能写 NULL，把"上游说是 default、我们没采到它的倍率"这个事实丢掉。
-- 存名字则两件事都在：名字有、解析不到，界面可以如实说"该分组未采到倍率"。
--
-- NULL = 还没采到（老库、或该站型不提供）。**不可当成 'default'** ——
-- 那是替上游做主，而站 B 恰好证明了 default 未必存在。

ALTER TABLE upstream_accounts
  ADD COLUMN account_group TEXT;

COMMENT ON COLUMN upstream_accounts.account_group IS
  '账号在上游的默认分组名（/api/user/self 的 group）。Key 的 group 为空时按它走。NULL = 未采到，不可当成 default。';
