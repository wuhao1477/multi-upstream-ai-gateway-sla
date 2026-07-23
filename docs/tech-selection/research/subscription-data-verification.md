# 订阅数据采集验证（FR-033/034 前置核查）

| 项目 | 内容 |
| --- | --- |
| 目的 | 验证实际接入渠道能否通过接口获取订阅计划的有效期、周期额度、已用额度 |
| 依据 | PRD v1.2 FR-033/034；OPEN-ISSUES 问题 1-4 |
| 状态 | ✅ 已完成（2026-07-23）：4 站登录态实测 + sub2api 源码级解析，三家族接口与订阅模型全部打通 |
| 参考项目 | [all-api-hub](https://github.com/qixing-jk/all-api-hub) |
| 详细结论 | [ISSUE-002 实测探测结果](../../issues/ISSUE-002-probe-results.md)、[采集适配器设计](../../issues/ISSUE-002-collector-adapter-design.md) |
| 责任人 | 运营（执行）、文档维护者（记录） |

## 1. 为什么要做

FR-033/034 要求持续获取每个订阅计划的有效期、周期额度、已用额度。约 20 个上游渠道多为 NewAPI/Sub2API 二次开发站点，尚无调查确认这些站点是否提供订阅信息查询接口。若多数站点查不到，"到期额度利用"（FR-036～039）只能依赖人工录入，这直接影响功能可行性判断，必须在开发计划前确认。

## 2. 验证方法

对每个选定渠道执行以下检查，逐项记录结果与证据（响应示例脱敏后存档）。接口清单和字段判定依据见 [ISSUE-002 调研结论](../../issues/ISSUE-002-upstream-data-collection.md)（已完成 all-api-hub 源码调研）：

1. 站点类型识别：`GET /api/status` 确认是 NewAPI、Sub2API 还是其他二开，记录版本线索；**同时确认站点是否开 WAF/Turnstile 盾**（服务端采集的主要障碍）。
2. 余额与用量：`GET /api/user/self`（`quota`）、`GET /api/log/self/stat`（精确消耗）；Sub2API 走 `/api/v1/auth/me`、`/api/v1/usage/stats`。
3. Key 级额度：`GET /api/token` 确认二开是否保留 `remain_quota`、`used_quota`、`expired_time`、`model_limits` 字段；Sub2API `/api/v1/keys` 的 `quota/quota_used/expires_at`。
4. 订阅/套餐端点：检查是否有类似 SharedChat `/frontend-api/vibe-code/quota` 的自定义订阅接口（含周期、重置时间、到期时间）；多数 NewAPI 系预期没有——此时周期额度/重置规则/月费按 FR-011 人工录入。
5. 余额差分兜底：连续两次查询余额做差分，验证可否推算消耗速度（服务 FR-036 预测）。
6. 采集限速：所有请求加最小间隔（参考 all-api-hub 的 minIntervalLimiter 设计），避免触发站点风控。

## 3. 判定标准

| 结果 | 判定 | 后续处理 |
| --- | --- | --- |
| 接口返回完整订阅字段 | 可自动采集 | 纳入异步采集路径，按参数 10 的频率（每小时）拉取 |
| 仅部分字段或仅页面可见 | 半自动 | 可采字段自动化，缺口按 FR-011 人工维护并标注来源 |
| 无任何订阅信息 | 仅人工 | 全量按 FR-011 人工维护 |

## 4. 采集不到时的降级规则（已在 PRD 框架内）

按 FR-011：允许人工维护并标明数据来源。人工录入的订阅数据设 7 天有效期；过期后系统自动按"订阅数据未知"降级——不再为消化额度加流量（不执行 FR-036～038 的订阅倾斜），仅保留渠道的普通调度资格。该规则与 OPEN-ISSUES 参数 10、参数 13 的建议一致。

## 5. 验证记录（2026-07-23 已完成）

| 渠道 | 站点类型/版本 | 订阅字段可得性 | 采集方式判定 | 证据 | 验证日期 |
| --- | --- | --- | --- | --- | --- |
| upstream-d.invalid | NewAPI `v1.0.0-rc.19` | 无订阅对象；余额/Key额度/倍率/价格全可得；周期额度来自签到 | 半自动（价格公开、账号级用系统令牌；订阅元数据 N/A） | 登录态实测：`/api/user/self`、`/api/token`、`/api/pricing`；额度换算 `used_quota/500000=¥20.77` 与页面一致 | 2026-07-23 |
| upstream-c.invalid | Sub2API `0.1.163` | 订阅端点齐全（本账号无生效订阅，回空）；分组/平台配额可得 | 可自动 | `/api/v1/groups/available`、`/api/v1/user/platform-quotas`、`/api/v1/keys` | 2026-07-23 |
| upstream-e.invalid | Sub2API（同构） | 同 molifang，端点一致；12 个分组 | 可自动 | `/api/v1/groups/available`（subscription_type/rate/limits）、`/api/v1/user/platform-quotas` | 2026-07-23 |
| upstream-f.invalid | 闭源自建（AMP Manager） | ✅ **完整订阅记录**（有生效订阅样本） | 可自动（专属适配器） | `/api/me/billing/state`（planName 每日90刀、expiresAt、limitMicros $90、fixedResetTime、remainingDays）、`/api/me/purchase/products`（16 套餐，priceCnyCent/durationDays） | 2026-07-23 |

**Sub2API 源码级补充**（`Wei-Shaw/sub2api` `63cef60`，弥补两站无生效订阅的空白）：ent schema 逐字段确认订阅模型——`subscription_plans`（price/currency/validity_days）、`user_subscriptions`（starts/expires_at、status、日/周/月 window+usage）、`group`（rate_multiplier、日/周/月 limit_usd、peak_rate）、`user_platform_quota`。共享额度按 `(user_id, group_id)` 聚合（FR-035），无超额计费概念，固定窗口重置。详见[采集适配器设计 §3.2](../../issues/ISSUE-002-collector-adapter-design.md)。

### 结论

三家族（NewAPI / Sub2API / ASXS 闭源）接口全部打通。订阅计划元数据在 Sub2API 系与 ASXS 系**可自动采集**，NewAPI 系无订阅对象（按 FR-011 人工录入 + 签到额度建模）。验证方法节所述"多站无接口需人工"的原预判被推翻——采集难点是**接口异构**而非数据缺失。
