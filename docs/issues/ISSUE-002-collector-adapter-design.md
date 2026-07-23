# 上游元数据采集：多平台适配器设计

| 项目 | 内容 |
| --- | --- |
| 日期 | 2026-07-23 |
| 来源 | ISSUE-002 源码调研 + 四站实测 |
| 定位 | 采集侧（异步控制路径）设计，不涉及请求链路 |
| 前置结论 | 上游站点异构程度高，**必须按站型分流**，不能一刀切 |

## 1. 已探明的站型分布

| 站点 | 站型 | 登录态 | 订阅数据 |
| --- | --- | --- | --- |
| upstream-d.invalid（redacted-channel-19） | NewAPI `v1.0.0-rc.19` | ✅ | ❌ 无订阅对象；周期额度来自**每日签到** |
| upstream-c.invalid（MoLiFang） | Sub2API `0.1.163` | ✅ | ✅ 端点齐全，当前账号无生效订阅 |
| upstream-e.invalid（redacted-channel-16） | **Sub2API**（同构） | ✅ | ✅ 端点齐全，当前账号无生效订阅 |
| upstream-f.invalid（ASXS Codex） | **独立闭源家族**（`/api/me/*`） | ✅ | ✅ **完整订阅模型，且有生效订阅** |

**关键发现 1：Sub2API 系高度同构。** molifang 与 hyhawang 的 localStorage 键名、JWT 载荷结构（`user_id/email/role/token_version/sid/bnd/exp/nbf/iat`）、24 小时有效期、`/api/v1/*` 端点族完全一致 —— 一个适配器可覆盖整个 Sub2API 家族，这大幅降低了 20 个上游的适配成本。

**关键发现 2：四站分属三个互不兼容的家族。** 接口命名空间分别是 `/api/user/self`（NewAPI）、`/api/v1/*`（Sub2API）、`/api/me/*`（ASXS），鉴权头、令牌存放位置、额度单位（quota 整数 / USD 浮点 / micros）全都不同。**"多平台适配器"不是过度设计，是必需品。**

**关键发现 3：订阅数据最完整的恰恰是闭源站。** 越是自建/闭源的平台，订阅模型越完整（ASXS 完整覆盖 FR-033）；反而通用开源的 NewAPI 完全没有订阅对象。这与原先"闭源站更难采集"的直觉相反，采集难点在**接口异构**而非**数据缺失**。

### 三家族速查

| 维度 | NewAPI | Sub2API | ASXS Codex |
| --- | --- | --- | --- |
| 命名空间 | `/api/user/*`、`/api/token` | `/api/v1/*` | `/api/me/*` |
| 令牌位置 | 需接口生成 | localStorage `auth_token` | localStorage `token` |
| 令牌有效期 | 长期（手动作废） | 24 小时 + refresh | 168 小时 |
| 附加头 | **必须** `New-API-User` | 无 | 无 |
| 额度单位 | quota 整数 ÷ `quota_per_unit` | USD 浮点 | micros（÷1e6） |
| 订阅模型 | ❌ 无 | ⚠️ 有端点，样本未populated | ✅ 完整 |

## 2. 适配器契约

每个站型实现同一接口，并**显式声明能力**；不支持的字段返回 `unsupported` 而非静默留空（与选型文档中 GatewayAdapter 的 `degraded_fields` 原则一致）。

```
CollectorAdapter:
  detect(baseUrl)                 -> 站型 + 版本
  authenticate(credential)        -> 会话句柄
  fetchAccount()                  -> 余额、已用、用户ID
  fetchKeys()                     -> Key 级额度、有效期、限流
  fetchGroups()                   -> 分组倍率、订阅类型、周期上限
  fetchSubscriptionQuotas()       -> 周期额度、已用、重置时间
  fetchPricing()                  -> 模型倍率
  capabilities()                  -> 各能力的 支持/降级/不支持
```

### 站型探测（接入任何新站点的第一步，公开无鉴权）

**复用性总原则**：NewAPI 与 Sub2API 是通用开源项目，**同族站点可复用同一适配器**，但因存在大量二开变体，接入时必须先探测确认站型再套用；ASXS 类闭源平台**一站一适配器，不可复用、不作探测基准**。

探测按顺序命中即停：

| 顺序 | 探测请求 | 命中特征 | 归类 |
| --- | --- | --- | --- |
| 1 | `GET /api/status` | 返回含 `quota_per_unit`、`turnstile_check`、`checkin_enabled` | **NewAPI 系** → 复用 NewApiAdapter |
| 2 | `GET /api/v1/settings/public` | 返回含 `site_name`、`turnstile_enabled`、envelope `{code,message,data}` | **Sub2API 系** → 复用 Sub2ApiAdapter |
| 3 | `GET /api/public/site-config` | 200 且 JWT `iss` 为 `ampmanager` | **ASXS 系** → 专属 AsxsAdapter |
| 4 | 全未命中 | —— | **未知家族** → 走 §3.4 接入流程，新建专属适配器 |

探测阶段零成本（全部公开端点），可安全地对 20 个上游批量跑一次，自动分桶。

> 二开兼容要点：NewAPI 系即使命中 §1，用户 ID 头名仍可能不同（`New-API-User`/`Veloera-User`/`voapi-user` 等），适配器内部对头名 fan-out；额度换算的 `quota_per_unit` 也逐站从 `/api/status` 读取，不写死。

## 3. 各站型实现要点

### 3.1 NewAPI 系（upstream-d.invalid 已验证）

| 项 | 结论 |
| --- | --- |
| 鉴权 | `Authorization: <token>` 或 `Bearer <token>` **均可**；**必须同时带 `New-API-User: <数字用户ID>`**，只带 Cookie 会 401 |
| 二开兼容 | 用户 ID 头名不统一，需 fan-out：`New-API-User` / `Veloera-User` / `X-Api-User` / `voapi-user` / `User-id` / `Rix-Api-User` / `neo-api-user` |
| 额度换算 | `金额 = quota / quota_per_unit`（upstream-d.invalid 为 500000）。**已实测验证**与页面显示一致 |
| 可采字段 | `/api/user/self`（quota、used_quota、request_count、group）、`/api/token`（expired_time、remain_quota、unlimited_quota、used_quota、model_limits）、`/api/log/self/stat`、`/api/pricing`（公开） |
| 订阅 | ❌ 无订阅对象。周期额度靠**签到**补充，须单独建模 |

> ⚠️ **`/api/user/token` 是"重新生成"而非"读取"** —— 实测每次调用都返回新令牌并**立即作废旧令牌**。采集器必须只在初始化时调用一次并持久化；重复调用会踢掉正在使用的令牌。本版本 `/api/user/self` 不回显 `access_token`，无法用"读不到再创建"的惰性策略。

### 3.2 Sub2API 系（molifang + hyhawang 实测 + 源码级解析）

| 项 | 结论 |
| --- | --- |
| 鉴权 | `Authorization: Bearer <JWT>`，JWT 有效期 **24 小时** |
| 凭证存储 | 站点前端存于 localStorage：`auth_token`、`refresh_token`、`token_expires_at`、`auth_user` |
| 续期 | `POST /api/v1/auth/refresh`，body `{refresh_token}` → 返回新的 access + refresh + expires_in |
| 订阅数据 | `/api/v1/subscriptions[/active/progress/summary]`、`/api/v1/groups/available`、`/api/v1/user/platform-quotas` |
| 容量数据 | `/api/v1/keys` 含 `rate_limit_5h/1d/7d`、`usage_5h/1d/7d`、`window_*_start`、`current_concurrency` |

#### 源码级订阅模型（sub2api `63cef60`，ent schema 为准 —— 比空账号更完整）

两站跑的正是开源 `Wei-Shaw/sub2api`（Go + ent ORM）。订阅相关有 4 张核心表，逐字段确认了 FR-033/034 覆盖：

| 表 | 字段 | 对应 FR-033/034 |
| --- | --- | --- |
| `subscription_plans`（可售套餐/商品） | `name`、`price`、`original_price`、`currency`、`validity_days`、`validity_unit`、`features`、`for_sale`、`group_id` | **固定费用**=price、**有效期**=validity_days×unit、**计费周期**、**支持模型/倍率**经 group_id 关联 |
| `user_subscriptions`（已购实例） | `user_id`、`group_id`、`starts_at`、`expires_at`、`status`(active/expired/suspended)、`daily/weekly/monthly_window_start`、`daily/weekly/monthly_usage_usd`、`assigned_by`、`notes` | **生效/到期时间**、**已用额度**（按日/周/月窗口）、**状态** |
| `group`（额度与倍率载体） | `rate_multiplier`、`peak_rate_enabled/peak_start/peak_end/peak_rate_multiplier`、`subscription_type`、`daily/weekly/monthly_limit_usd`、`rpm_limit`、`platform`、`is_exclusive` | **周期包含额度**（上限）、**倍率**、**高峰倍率**、**重置规则**（窗口） |
| `user_platform_quota`（按平台额度视图） | `platform`、`daily/weekly/monthly_limit_usd` + `_usage_usd` + `_window_start` | 平台级周期额度 + 已用 + 重置窗口 |

**三个源码级关键结论**：

1. **共享额度机制（FR-035）确证**：订阅用量按 `(user_id, group_id)` 聚合，账务入口 `UpdateSubscriptionUsage(userID, groupID, cost)`。同一用户同一 group 下的**多个 API Key 天然共享同一份订阅额度**——采集器必须按 (用户, group) 归集用量，**不能按 Key 归集**，否则会重复计算。

2. **无超额计费概念**：sub2api 订阅状态只有 active/expired/suspended，schema 中**无 overage/超额字段**。周期额度用尽即在窗口内阻断，直到 `subscription_expiry_service` 按 `*_window_start` 重置（日/周/月固定窗口）。故 FR-033 的"超额计费规则"对 sub2api 系为**不适用**，采集器据实登记为"无超额、用尽即阻断"。

3. **重置是固定窗口**：`ResetDailyUsage/ResetWeeklyUsage/ResetMonthlyUsage` + `window_start` 时间戳，对应实测 API 的 `*_window_resets_at`。可直接用于 FR-036 到期未用预测。

> 采用源码解析而非账号实测的收益：molifang/hyhawang 两个账号均无生效订阅，API 只回空数组；ent schema 把所有字段（含空账号看不到的 window/usage/shared 语义）一次性确认，粒度更细、更可靠。

### 3.3 ASXS Codex 系（upstream-f.invalid 已验证）——订阅数据最完整的家族

第三个独立家族，与前两者无任何接口重叠。**这是目前唯一采到完整订阅记录的站点，可作为订阅台账建模的基准样本。**

| 项 | 结论 |
| --- | --- |
| 底层产品 | JWT `iss: "ampmanager"`、`aud: "ampmanager-users"` —— 基于闭源"AMP Manager"平台搭建，**非任何开源项目** |
| 命名空间 | `/api/me/*`（私有）、`/api/public/*`（公开） |
| 鉴权 | JWT `Authorization: Bearer`，存于 localStorage 键 `token`；**有效期 168 小时（7 天）**，无 cookie |
| 额度单位 | micros（`limitMicros: 90000000` = $90）；`balanceUsd` 另有字符串形式 |
| 核心端点 | `/api/me/billing/state`、`/api/me/purchase/products`、`/api/me/balance`、`/api/me/profile` |

> ⚠️ **本适配器专属，其他渠道不可复用。** ASXS 是闭源自建平台，`/api/me/*` 命名空间、micros 额度单位、JWT claim 结构均为其独有，与 NewAPI/Sub2API 零重叠。必须单独实现一个 `AsxsCollectorAdapter`，且不作为其他站点的探测基准。

### ASXS JWT 续期机制（专项实测结论）

用户提出"asxs 续期不了解，或用账号密码自动续期"，实测已查清：

| 验证项 | 结果 |
| --- | --- |
| localStorage 有无 refresh_token | ❌ 只有 `token`，无 refresh 凭证 |
| JWT 载荷有无 refresh claim / sid | ❌ 均无（仅 `user_id/username/iss/aud/exp/nbf/iat`） |
| `/api/me/refresh`、`/api/me/renew`、`/api/me/session/refresh` | ❌ 全部 404，无仪表盘续期端点 |
| `/api/auth/refresh`、`/api/auth/renew` | 存在但属**另一套 API-Key 命名空间**，拒绝 JWT（报"API Key 无效"），非仪表盘续期 |
| 页面加载时是否自动续期 | ❌ 实测刷新后 17 个请求全是 `/api/me/*` 数据拉取，**无任何 auth/refresh/login 调用** |

**结论：ASXS 没有任何静默续期机制。** 7 天 JWT 是固定有效期的会话令牌，到期后唯一续法是**重新登录**（账号密码 → 登录端点 → 新的 7 天 JWT）。

**采集器续期方案（对应用户的"账号密码自动续期"，已抓取登录端点）**：
- **登录端点已确认**：`POST /api/manage/auth/login`（第四个命名空间 `/api/manage/auth/*`，管理面登录；难怪 `/api/auth/*` 与 `/api/me/*` 下都找不到）。请求体为用户名+密码，成功返回 7 天 JWT，前端写入 localStorage 键 `token`。无效凭证返回 401。
- 保存该站账号密码（一期本就明文存储，见 DECISIONS 遗漏 4），采集器检测 JWT 剩余有效期 < 阈值（建议 < 1 天）时调 `POST /api/manage/auth/login` 重新登录换取新 JWT。
- 7 天有效期意味着重登频率极低（每周 1 次），风控压力小。
- 与 Sub2API 的差异：Sub2API 有 refresh_token 可无密码续期；**ASXS 必须持有账号密码**才能续期。这是闭源平台的固有约束。
- ASXS 命名空间全貌：`/api/public/*`（公开）、`/api/me/*`（用户数据，Bearer JWT）、`/api/manage/auth/*`（登录）、`/api/auth/*` 与 `/api/user/*`（API-Key 代理鉴权，与仪表盘 JWT 不通用）。

实测订阅记录（`/api/me/billing/state`）——**FR-033 要求的字段几乎全覆盖**：

| FR-033 要求 | ASXS 字段 | 实测值 |
| --- | --- | --- |
| 计划标识 | `planId` / `planName` | 每日90刀 |
| 生效时间 | `startsAt` | 2026-06-24 |
| 到期时间 | `expiresAt` | 2026-07-26 |
| 计费周期 | `limits[].limitType` + `windowMode` | daily / fixed |
| 包含额度 | `limits[].limitMicros` | 90000000（$90/日） |
| 重置规则 | `limits[].fixedResetTime` | `00:00` |
| 续订状态 | `renewalRule`（products） | 支持多订阅，可续期同套餐 |
| 状态 | `status` | active |
| 已用/剩余 | `usedMicros` / `leftMicros` | 0 / 90000000 |
| 到期紧迫度 | `remainingDays` | 2 |

**固定费用**来自 `/api/me/purchase/products`（16 个套餐）：`priceCnyCent`、`durationDays`、`subscriptionPlanName`、`renewAllowed`。例如"日卡 $10"= `priceCnyCent 65`、`durationDays 1` —— **这正是参数 14 双倍率计算所需的全部输入**。

另有两个本项目未预见但很有价值的字段：

- `primarySource: "subscription"` / `secondarySource: "balance"` —— 平台**自身**的额度消耗优先级。采集器必须读取它，否则会错判"订阅额度到底会不会被消耗"。
- `dailyReset` —— 该平台支持在用量超 `usageThresholdPercent`(90%) 时手动重置每日额度（`dailyLimit: 4` 次/日）。这是一种可主动争取的额外额度，属于 FR-036 预测模型的外生变量。

> 实测当下即是一个真实的"额度浪费"案例：该订阅 2 天后到期，当日 $90 额度 `usedMicros = 0`。这正是 FR-036/FR-039 要处理的场景，可直接作为验收用例。

### 3.4 未知家族的接入流程

1. 探测是否为已知家族（依次比对 `/api/status`、`/api/v1/settings/public`、`/api/public/site-config`）
2. 未命中：抓取其前端实际调用的接口（`performance.getEntriesByType('resource')` 思路可在浏览器内快速取样），人工确认字段语义后写专用适配器
3. 确实无接口：按 FR-011 人工录入，标注数据来源，7 天有效期，过期按"订阅数据未知"降级

## 4. 凭证生命周期管理（核心风险区）

实测暴露出两类站点**都有"凭证互斥作废"风险**，这是采集器最容易出事的地方：

| 站型 | 风险 | 对策 |
| --- | --- | --- |
| NewAPI | `/api/user/token` 重复调用作废旧令牌 | 只在接入时生成一次并持久化；**禁止**在运行时重新生成；令牌失效需人工介入或走登录流程 |
| Sub2API | refresh 会轮换 `refresh_token`，并发刷新互相作废 | **按账号加互斥锁**串行刷新（all-api-hub 的 `withSub2ApiAuthMutationLock` 即为此）；刷新结果立即持久化 |

### Sub2API 会话续期的四层策略（借鉴 all-api-hub）

1. **主动刷新**：到期前 120 秒（`SUB2API_TOKEN_REFRESH_BUFFER_MS`）用 refresh_token 换新
2. **被动刷新**：请求返回 401 时用 refresh_token 再试一次
3. **浏览器重同步**：refresh_token 也失效时，从已开标签页或临时窗口读站点新签发的 JWT（依赖站点浏览器会话 Cookie）
4. 全部失败 → 抛"需要重新登录"错误

**服务端可移植性**：第 1、2 层是纯 HTTP，可 1:1 移植；**第 3 层不可移植**（依赖浏览器会话）。服务端等价方案是保存站点登录 Cookie 或以账号密码重新登录换 JWT，作为第 3 层替代。

### 设备绑定：已确认无风险

JWT 载荷含 `sid`（会话 ID）与 `bnd` 字段，原疑为设备绑定。**用户已确认 Sub2API 未开启设备绑定** —— 服务端采集器换 IP/UA 用 refresh_token 续期不会被拒，该风险解除。若未来某上游开启了绑定，再按站单独处理。

## 5. 通用防护

- **限速**：同站点请求强制最小间隔（参考 all-api-hub `minIntervalLimiter`），避免触发风控；频率上限见 OPEN-ISSUES 参数 10
- **无盾确认**：已实测 upstream-d.invalid `turnstile_check: false`、molifang `turnstile_enabled: false`；新站点接入前必须先确认此项，开盾站点服务端采集不可行
- **降级一致性**：任何字段采集失败都落到 FR-011（人工录入 + 7 天有效期 + 过期按未知降级），不允许用陈旧数据驱动订阅倾斜

## 6. 对 PRD 与既有决策的反馈

1. **参数 14 双倍率设计已被实测证实可行且必要**：ASXS 同时提供 `priceCnyCent`+`durationDays`（算用满倍率）和 `usedMicros`（算实际倍率）。建议把双倍率字段正式写入 FR-033 订阅台账。
2. **FR-033 应补两个字段**（实测发现、原 PRD 未覆盖）：
   - `额度消耗优先级`（对应 ASXS `primarySource/secondarySource`）——不读它会错判订阅额度是否真被消耗；
   - `可主动重置额度`（对应 ASXS `dailyReset`）——属于可争取的额外额度，影响到期浪费预测。
3. **NewAPI 系的"签到额度"应建模为周期额度补充**（upstream-d.invalid 本月签到得 ¥160），否则该类站点的额度预测会系统性偏低。
4. **额度单位换算必须适配器内完成**：三家族分别是 `quota/quota_per_unit`、USD 浮点、micros/1e6。统一口径为美元（对应参数 9 决策）。

## 7. 待办

1. ~~有生效订阅的 Sub2API 账号验证字段~~ ✅ 改用 sub2api 源码 ent schema 逐字段确认（见 §3.2），比空账号更完整，无需生效订阅。
2. ~~验证 Sub2API 的 `bnd` 是否构成设备绑定~~ ✅ 用户已确认未开启，风险解除。
3. ~~验证 ASXS 续期机制与登录端点~~ ✅ 已查清：无静默续期，登录端点 `POST /api/manage/auth/login`，靠账号密码重登换 7 天 JWT。
4. ~~币种折算~~ ✅ 已决策一期 1:1（币种仅名称）。

**采集侧调研至此全部闭环，剩余仅 Docker 实跑验证（[ISSUE-001](./ISSUE-001-tech-assumption-verification.md)）与登录响应字段的实现期一次性确认。**

## 8. 三家族最终定论

| | NewAPI | Sub2API | ASXS Codex |
| --- | --- | --- | --- |
| 项目性质 | 开源通用 | 开源通用 | **闭源自建（AMP Manager）** |
| 适配器复用 | ✅ 同族复用（需探测匹配二开） | ✅ 同族复用（molifang/hyhawang 已证同构） | ❌ **一站一适配器，不可复用** |
| 令牌与续期 | 系统访问令牌，长期，初始化一次生成 | JWT 24h + refresh_token 无密码续期 | JWT 7d，无 refresh，须账号密码重登 `POST /api/manage/auth/login` |
| 订阅数据 | 无（靠签到补额度） | 有端点（本样本未 populated） | ✅ 完整，建模基准 |
| 已实测站点 | upstream-d.invalid | molifang、hyhawang | asxs.top |
