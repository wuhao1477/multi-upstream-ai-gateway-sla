-- 019：channels.base_url 唯一约束
--
-- Codex 二次评审（2026-09-01）报的 [high]：**渠道身份没有库级唯一性**。
-- 两处写入都是"先查再插"：
--   · createChannel 直接 INSERT，不查重；
--   · importOne 先 ListChannels 找同 base_url、找不到才 INSERT。
-- 于是"同地址只该有一个渠道"这条规则只在**单请求内串行**成立。两个并发导入、
-- 或客户端在 201 响应丢失后重试，都能各自观察到"不存在"然后各插一条 —— 台账里
-- 出现重复渠道、重复账号、重复凭证，而库里**没有 DELETE 渠道的入口**。
--
-- read-then-insert 防不住并发，这不是实现没写好，是**这个形状本身防不住**：
-- 两个事务在 REPEATABLE READ 以下都看不到对方未提交的插入。唯一能防住的地方
-- 只有库。
--
-- ⚠️ 加约束前已在真库核过（只读）：
--      raw 重复组数 0 / 去尾斜杠后 0 / lower+去尾斜杠后 0
--      base_url 为空或非 http(s) 的行 0 / 带尾斜杠的行 0
--    65 行渠道全部满足，故这条约束**不需要数据清理**，加上去就生效。
--
-- ⚠️ 规范化的责任在写入侧，不在这条约束：约束认的是**字面值**，所以
--    `https://x.com` 与 `https://x.com/` 会被当成两个不同的值。三处写入路径
--    都已统一 `strings.TrimRight(url, "/")`（createChannel / importOne /
--    patchChannel —— 最后一个是 2026-09-01 才补的，它此前连 http 前缀都不查）。
--    没做 lower()：host 大小写不敏感但 path 敏感，一刀切小写会改变语义；
--    真库 0 行大小写重复，这个残留缺口记在这里而不是用一个错的规范化盖住。
--
-- ⚠️ 为什么是新增 019 而不是回去改 001：
--    migrate.go 的 checksum 契约 —— 已应用的迁移文件 checksum 不一致时直接
--    return error 且不执行任何迁移。真库已应用 18 个文件。同 016/017/018 头部。

ALTER TABLE channels
  ADD CONSTRAINT channels_base_url_key UNIQUE (base_url);

COMMENT ON COLUMN channels.base_url IS
  '上游根地址，采集与（P2 起）转发都按它出站；全库唯一（019）。'
  '写入侧须先去掉尾斜杠再落库 —— 约束认字面值，尾斜杠会绕过它。'
  '必须经 internal/admin.validateBaseURL 校验（scheme 为 http/https 且 host 非空）；'
  '三处写入路径 createChannel / importOne / patchChannel 都在校验之后才写。';
