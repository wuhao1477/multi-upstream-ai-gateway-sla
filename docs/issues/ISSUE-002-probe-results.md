# ISSUE-002 附：两站实测探测结果与取数方案

| 项目 | 内容 |
| --- | --- |
| 探测日期 | 2026-07-22 |
| 探测方式 | 仅访问公开端点（未登录、未使用任何凭证） |
| 渠道 1 | https://upstream-c.invalid —— Sub2API 系 |
| 渠道 2 | https://upstream-d.invalid —— NewAPI 系 |
| all-api-hub 参考版本 | v3.52.0 `6236dca3`（AGPL-3.0） |

## 1. 探测结论速览

| 项 | upstream-d.invalid (NewAPI) | upstream-c.invalid (Sub2API) |
| --- | --- | --- |
| 站型确认 | ✅ NewAPI `v1.0.0-rc.19-i18nfix.2` | ✅ Sub2API `0.1.163`，自述"Subscription to API Conversion Platform" |
| WAF / 人机验证 | `turnstile_check: false` ✅ 无盾 | `turnstile_enabled: false` ✅ 无盾 |
| 密码登录 | `password_login_enabled: true` ✅ | 支持（`password_reset_enabled: false`） |
| 公开可采（无需登录） | `/api/status`、`/api/pricing`（全量模型倍率/分组倍率/缓存倍率） | `/api/v1/settings/public` |
| 需登录令牌 | `/api/user/self`、`/api/token`、`/api/log/self/stat` 等（未登录返回空/401） | `/api/v1/auth/me`、`/api/v1/keys`、`/api/v1/groups/rates`（未登录返回空/401） |

**两站均无 Turnstile/WAF，是服务端采集最友好的情况**——不需要 all-api-hub 的临时浏览器过盾机制，只需实现它记录的鉴权+刷新流程即可。

关键增量：upstream-c.invalid 本身就是**订阅转 API 平台**，`purchase_subscription_enabled` 字段存在（当前 false），说明它有订阅计划对象，FR-033/034 想要的订阅字段大概率能从登录态接口直接拿到（待令牌确认）。upstream-d.invalid 的 `/api/pricing` 无需登录即返回完整倍率，可直接服务价格采集（FR-010）。

## 2. 取令牌的三种方式对比（核心评估）

### upstream-d.invalid（NewAPI 系）

| 方式 | 拿什么 | 能读什么 | 有效期/可靠性 | 适合采集器？ |
| --- | --- | --- | --- | --- |
| **系统访问令牌 access_token**（推荐） | 登录后 `/api/user/self` 返回的 `access_token` 字段；为空则调 `/api/user/token` 自动创建 | 余额、日志、Key 列表、分组、订阅/额度等**全部管理数据** | **长期有效**，稳定到用户在后台手动重置为止 | ✅ 最佳。all-api-hub 用的就是这个 |
| Cookie / session | 登录后的会话 Cookie | 同上 | 会过期，需重登，脆弱 | ⚠️ 不推荐用于长驻采集 |
| API Key（sk-...） | 后台手动建的调用密钥 | 只能打 `/v1/chat/completions`，**读不了**账户/额度/订阅 | 长期有效 | ❌ 不能用于取数 |

> 调用时除 `Authorization: <access_token>` 外，NewAPI 系还需 `New-API-User: <数字用户ID>` 头（all-api-hub 对多种二开 fan-out 了 `New-API-User`/`Veloera-User`/`voapi-user` 等多个头名）。所以除令牌外还需**数字用户 ID**。

### upstream-c.invalid（Sub2API 系）

| 方式 | 拿什么 | 有效期/可靠性 | 适合采集器？ |
| --- | --- | --- | --- |
| **JWT access_token + refresh_token**（唯一可行） | 登录后从浏览器 localStorage 取；access_token 短期（带 `expires_in`），refresh_token 长期 | access 短命，需在到期前 ~2 分钟用 `/api/v1/auth/refresh` 刷新；refresh_token 长期有效，除非登出全部/改密 | ✅ 需实现刷新循环。all-api-hub 正是如此（`SUB2API_TOKEN_REFRESH_BUFFER_MS = 120s`） |
| API Key（sk-...） | 后台建的调用密钥 | 长期 | ❌ 读不了订阅/额度管理数据 |

> Sub2API **没有** NewAPI 那种"系统长期令牌"概念。采集器必须保存 `refresh_token` 并自建刷新循环；这是它与 NewAPI 最大的取数差异。

## 3. 能否直接借用 all-api-hub 的能力？

**不能直接跑，但设计可复用：**

- all-api-hub 是**浏览器扩展**（WXT + React），鉴权依赖浏览器登录态、localStorage 和临时窗口过盾——服务端采集器无法直接加载运行。
- 但本项目两站都**无盾**，用不上它最复杂的过盾部分；剩下的鉴权+刷新流程本身简单且已在源码中读清（见 ISSUE-002 接口清单），服务端重写即可。
- 可复用的具体设计：① NewAPI 的 `getOrCreateAccessToken`（先读 `/api/user/self`.access_token，空则 `/api/user/token` 创建）；② 多头名 fan-out 兼容二开；③ Sub2API 的到期前 2 分钟主动刷新；④ `minIntervalLimiter` 同站点请求最小间隔防风控；⑤ 每日余额快照做差分推算消耗。
- **许可证 AGPL-3.0**：只借鉴接口清单与设计思路，不复制代码（本项目个人/内部使用，风险低）。

## 4. 令牌有效期与可靠性总评

| 令牌 | 时效 | 断裂风险 | 采集器对策 |
| --- | --- | --- | --- |
| NewAPI 系统访问令牌 | 长期 | 用户后台手动重置；部分二开可能对令牌加有效期 | 用一个**专用采集账号**建令牌，不与日常混用；令牌失效时告警并触发重新登录取新令牌 |
| NewAPI Cookie | 数小时~数天 | 会话过期 | 不作主方案，仅在无法拿系统令牌时兜底 |
| Sub2API refresh_token | 长期 | 改密、登出全部设备、服务端吊销 | 持久化 refresh_token；实现到期前刷新；刷新失败告警并要求重新登录 |
| Sub2API access_token | 短期（分钟~小时级） | 正常过期 | 自动刷新循环，业务无感 |

## 5. 建议的取数落地方式（待用户确认凭证提供方式）

推荐 **Option A：用户手动提供令牌**（最干净、不暴露密码）：
- upstream-d.invalid：登录后台 → 个人设置取**系统访问令牌**（或让系统自动创建）+ 页面上的**数字用户 ID**。
- upstream-c.invalid：登录后按 F12 → Application → Local Storage 复制 **access_token 和 refresh_token**（可指导操作）。

备选 **Option B：用户提供账号密码**，由采集器服务端脚本模拟登录取令牌（两站都支持密码登录）。缺点是需存储明文密码（本项目一期本就明文，但令牌方案仍更优）。

拿到凭证后即可实测：验证订阅字段可得性、Key 级 `expired_time/remain_quota`、余额差分推算，并填写 [订阅数据采集验证](../tech-selection/research/subscription-data-verification.md) 记录表。

---

# 6. 登录态实测结果（2026-07-23）

实测方式：用户已登录的浏览器会话，在页面上下文内调用各站鉴权接口。**本文档不含任何令牌、密码或 Key 明文。**

## 6.1 鉴权实测

| 项 | upstream-d.invalid (NewAPI v1.0.0-rc.19) | upstream-c.invalid (Sub2API 0.1.163) |
| --- | --- | --- |
| 鉴权方式 | 会话 Cookie **+ `New-API-User: <数字用户ID>` 头** | JWT `Authorization: Bearer` |
| 关键坑 | **只带 Cookie 会 401**，必须附用户 ID 头 —— all-api-hub 的多头名 fan-out 正是为此 | 令牌存于 localStorage：`auth_token`、`refresh_token`、`token_expires_at` |
| 令牌时效 | 本版本 `/api/user/self` **不返回 `access_token` 字段**，需调 `/api/user/token` 创建；创建后长期有效 | JWT 实测有效期 **24 小时**（`exp - iat = 86400`）；到期前用 `/api/v1/auth/refresh` 刷新 |
| 长期凭证 | 系统访问令牌（需手动/接口创建） | `refresh_token`（长期） |
| 风险点 | Cookie 会过期，不适合长驻采集 | JWT 载荷含 `sid`、`bnd`、`token_version`；**`bnd` 疑似设备/环境绑定，服务端换 IP/UA 是否失效需验证** |

## 6.2 upstream-d.invalid（NewAPI 系）数据可得性

- `/api/user/self` ✅：`quota` 129614735、`used_quota` 10385265、`request_count` 571、`group`。
- **额度换算已验证**：`/api/status` 的 `quota_per_unit = 500000`，`used_quota / 500000 = ¥20.77`，与页面显示完全一致 → 换算公式可靠。
- `/api/token` ✅：Key 级字段完整保留 —— `expired_time`、`remain_quota`、`unlimited_quota`、`used_quota`、`model_limits`、`model_limits_enabled`、`allow_ips`、`group`。**二开未阉割关键字段。**
- `/api/pricing` ✅ 公开无需登录：全量 `model_ratio`、`group_ratio`、`cache_ratio`、`completion_ratio`。
- ❌ **无订阅计划对象**（钱包语义）。该站的周期性额度来自**每日签到**（`checkin_enabled: true`，本月已获 ¥160）——这构成事实上的周期额度补充，应纳入额度预测模型。

## 6.3 upstream-c.invalid（Sub2API 系）数据可得性 —— 关键发现

订阅端点族真实存在且可访问：

| 端点 | 状态 | 价值 |
| --- | --- | --- |
| `/api/v1/groups/available` | ✅ 200 | **直接给出订阅计划元数据**：`subscription_type`、`rate_multiplier`、`daily/weekly/monthly_limit_usd`、`peak_rate_enabled/peak_start/peak_end/peak_rate_multiplier`、`rpm_limit`、`is_exclusive`、`fallback_group_id` |
| `/api/v1/user/platform-quotas` | ✅ 200 | **正是 FR-034 所需**：按平台给出 `daily/weekly/monthly_limit_usd`、`*_usage_usd`、`*_window_resets_at`（周期额度 + 已用额度 + 重置时间） |
| `/api/v1/keys` | ✅ 200 | `quota`、`quota_used`、`expires_at`，另有 `rate_limit_5h/1d/7d`、`usage_5h/1d/7d`、`window_*_start`、`current_concurrency` —— 直接服务 FR-020～032 容量建模 |
| `/api/v1/subscriptions`、`/api/v1/subscriptions/active` | ✅ 200 但为空 | 该账号当前无生效订阅 |
| `/api/v1/plans`、`/api/v1/user/subscription` | ❌ 404 | 可能需参数或管理员权限 |

实测分组倍率样本（`rate_multiplier`）：Plus稳定 0.055、稳定Pro 0.15、Kiro高缓 0.04、CCMAX满血 …，`platform` 区分 openai/anthropic。这正是**双倍率设计**（参数 14）所需的输入。

## 6.4 对原判断的修正

ISSUE-002 源码调研原结论是"订阅计划元数据多数站点需人工录入"。实测后**按站型分化**：

- **Sub2API 系（molifang）**：订阅计划元数据、周期额度上限、已用额度、窗口重置时间**全部可自动采集**，显著优于原预判。FR-033/034 对该类站点基本可全自动。
- **NewAPI 系（upstream-d.invalid）**：确认无订阅对象，维持原结论 —— 按 FR-011 人工录入 + 7 天有效期降级；但余额、Key 额度、倍率、价格全自动，且签到额度需纳入模型。

结论：**采集策略必须按站型分流，不能一刀切**。

## 6.5 尚未验证 / 待办

1. **订阅记录字段的完整形态未验证** —— 当前账号无生效订阅，`/api/v1/subscriptions` 返回空。需要一个**有生效订阅的账号**才能确认 FR-033 要求的固定费用、生效/到期日期、重置规则、超额计费等字段是否齐全。
2. **NewAPI 系统访问令牌尚未创建** —— `/api/user/token` 会在账户中创建一个长期凭证，属于账户状态变更，待用户确认后再执行。
3. **`bnd` 设备绑定风险未验证** —— 需在服务端环境（不同 IP/UA）用同一 refresh_token 试刷新，确认是否被拒。
