-- 030：channel_model_catalog 补 vendor_icon
--
-- 上游 /api/pricing 的 `vendors[]` 每项除了 name 还带 `icon`，实测取值是
-- lobehub 的图标名（2026-09-15 实测某真实站点 35 家发行方、27 个不同 icon 名：
-- `OpenAI` / `Claude.Color` / `Zhipu.Color` / `Qwen.Color` …）。
--
-- 存它而不是在前端按名字猜：同一家公司在同一个站里有好几个名字
-- （实测 Alibaba / 阿里巴巴 / Alibaba (China) / Alibaba Token Plan 四个名字
-- 共用 `Qwen.Color` 一个图标），按名字猜就要手写一张别名表，而那张表
-- 每加一个站点就可能漏一行 —— 漏了是静默的（退回字母块）。
-- icon 是上游自己的声明，跟着 vendor_name 一起存最省事也最准。
--
-- NULL = 上游未声明（老版本没这个字段）。界面据此退回名字取色的字母块，
-- **不是留空** —— 空格子读起来像"没有发行方"，而那是另一回事。

ALTER TABLE channel_model_catalog
  ADD COLUMN vendor_icon TEXT;

COMMENT ON COLUMN channel_model_catalog.vendor_icon IS
  '发行方图标名（上游 vendors[].icon，lobehub 图标名）。NULL = 未声明，界面退回字母块。';
