# ISSUE-002：上游订阅/渠道数据自动采集调研（含 all-api-hub 参考）

| 项目 | 内容 |
| --- | --- |
| 状态 | ✅ 全流程完成（2026-07-23）：源码调研 + 4 站登录态实测 + sub2api 源码级订阅模型解析 |
| 调研版本 | all-api-hub v3.52.0 `6236dca3`；sub2api `63cef60`（AGPL/LGPL，仅借鉴不复制） |
| 来源 | OPEN-ISSUES 问题 1-4；2026-07-22 确认 |
| 关键输入 | 上游渠道大多数为 NewAPI/Sub2API 二开站点，另有闭源自建平台（ASXS Codex） |
| 参考项目 | [qixing-jk/all-api-hub](https://github.com/qixing-jk/all-api-hub) |

> **本文档是采集调研的第一阶段记录（all-api-hub 源码 + 两站公开端点）。后续进展见：**
> - [ISSUE-002 两站实测探测结果](./ISSUE-002-probe-results.md)（4 站登录态实测、令牌时效）
> - [ISSUE-002 多平台采集适配器设计](./ISSUE-002-collector-adapter-design.md)（三家族适配器、sub2api 源码级订阅模型、ASXS 续期）
>
> 下方"待凭证"等表述已被上述文档中的实测结论取代，保留于此仅作调研过程留痕。

## 背景

FR-033/034 要求持续获取订阅计划的有效期、周期额度、已用额度。原预判是"多数二开站点无标准接口，只能人工录入"。但 all-api-hub 已对 NewAPI 系站点实现了**自动创建 Key、自动获取渠道模型、自动签到、自动获取 Key 分组**等能力，说明用户侧接口自动采集的可行性可能高于原预判，值得先调研它的实现方式再下结论。

## 调研任务

1. 阅读 all-api-hub 源码，整理它对 NewAPI/Sub2API 系站点使用的接口清单：登录/鉴权方式、余额与额度查询、Key 与分组管理、签到、模型列表；记录各接口的请求路径、参数和响应字段。
2. 评估这些接口中哪些能覆盖 FR-033/034 需要的字段（订阅有效期、周期额度、已用额度、重置规则），哪些字段仍缺失。
3. 评估二开站点的差异风险：接口路径/字段被改名、鉴权差异、风控（频繁调用被封）。
4. 输出结论：可复用的采集方式清单 + 缺口清单，更新[订阅数据采集验证](../tech-selection/research/subscription-data-verification.md)的验证方法。
5. 待项目负责人提供 2～3 个实际渠道后，按验证文档实测并填写记录表。

## 降级预案（已确认）

采集不到的字段按 FR-011 人工维护并标注来源；人工数据 7 天有效期，过期后按"订阅数据未知"降级（不再为消化额度加流量）。

## 调研结论（2026-07-22）

### 1. 站点适配面：比预期广得多

all-api-hub 账号侧适配 17 种站型：OneAPI、NewAPI、AnyRouter、Veloera、OneHub、DoneHub、VAPI、VoAPI(v1/v2)、SuperAPI、RixAPI、NeoAPI、WongGongyi、Sub2API、AiHubMix、SharedChat 等。二开差异用"NewAPI 家族 + 变体覆盖"模式管理（`newApiFamily/variants/{veloera,oneHub,doneHub,wong,anyrouter}.ts`），证明差异真实存在但工程上可控。它还有管理侧适配（NewAPI、Veloera、DoneHub、Octopus、**AxonHub**、ClaudeCodeHub），含渠道管理 `/api/channel/`。

### 2. NewAPI 系可自动采集的接口清单（已验证于源码）

| 用途 | 接口 | 关键字段 |
| --- | --- | --- |
| 站点探测 | `GET /api/status` | 站点类型/版本线索 |
| 账号余额 | `GET /api/user/self` | `quota`（钱包余额）、`access_token` |
| 用量统计 | `GET /api/log/self`、`GET /api/log/self/stat` | 精确今日消耗 `quota`、请求/Token 数 |
| Key 管理 | `GET/POST /api/token`、`PUT/DELETE /api/token/:id`、`GET /api/token/{id}/key` | 自动建 Key；Key DTO 含 `remain_quota`、`used_quota`、`unlimited_quota`、`expired_time`、`group`、`model_limits/models` |
| 分组倍率 | `GET /api/user/self/groups`、`/api/group`、`/api/user_group_map` | 用户组、分组倍率 |
| 模型价格 | `GET /api/pricing` | 模型倍率/价格（服务 FR-010 系采集） |
| 模型列表 | `GET /api/user/models`、`/api/available_model` | 可用模型 |
| 签到 | `POST /api/user/checkin`、`GET /api/user/check_in_status` | 含 Turnstile 处理 |

Sub2API 系（envelope `{code,message,data}`）：`/api/v1/auth/me`（balance）、`/api/v1/keys`（**`quota`、`quota_used`、`expires_at`**、group.`rate_multiplier`）、`/api/v1/groups/available|rates`、`/api/v1/usage/stats`、`/api/v1/auth/refresh`。

### 3. FR-033/034 字段覆盖判定

| FR-033/034 字段 | 自动可得性 |
| --- | --- |
| 已用额度、剩余额度 | ✅ NewAPI `quota`+日志统计；Sub2API `quota/quota_used`；Key 级 `remain_quota/used_quota` |
| 有效期/到期时间 | ⚠️ 部分：Key 级 `expired_time`/`expires_at` 可得；**账号级订阅计划到期时间在 NewAPI 系标准接口中不存在** |
| 周期额度、重置规则、固定月费、超额计费 | ❌ NewAPI 系标准接口无订阅计划对象（钱包语义，非订阅语义），需人工录入 |
| 分组倍率、支持模型 | ✅ 接口可得 |
| 完整订阅 payload | ✅ 仅个别站型：SharedChat `/frontend-api/vibe-code/quota` 返回 `period`、`periodResetTime`、`expireTime`、`limit`、`usedAmount`、`remainingAmount`、`isActive`——证明部分站点有完整订阅接口，须逐站确认 |

**结论：自动采集可行性高于原预判，但结构性缺口明确**——"已用/剩余/消耗速度"可全自动（FR-034 大部分满足，FR-036 预测输入充足）；"订阅计划元数据"（月费、周期、重置规则）多数站点仍需按 FR-011 人工录入（7 天有效期降级预案维持不变，但适用范围缩小到计划元数据）。查不到已用额度的站点可用**余额差分**推算消耗（all-api-hub 的 dailyBalanceHistory 每日余额快照即此思路）。

### 4. 关键风险与工程借鉴

1. **all-api-hub 是浏览器扩展**，依赖用户登录态（Cookie/access_token 双模式、按账号 Cookie 隔离）；自研采集器是服务端，应走 NewAPI 的 `access_token` 头认证。**WAF/Turnstile 站点是服务端采集的主要障碍**——all-api-hub 用临时浏览器窗口过盾（tempWindowFetch/shield bypass），服务端无法复用；渠道实测时必须确认目标站点是否开盾。
2. **限速防风控**：其 minIntervalLimiter/siteRequestLimiter 对同站点请求强制最小间隔——采集器必须同样限速（参数 10 的频率上限之内再加请求间隔），避免账号被风控。
3. **许可证**：AGPL-3.0。只借鉴接口清单和设计思路，不复制代码（本项目个人/内部使用，风险本就低）。

### 5. 对后续实测的修正

实测 2～3 个渠道时，验证重点从"有没有接口"改为：① 站点是否开 WAF/盾；② `/api/token` 的 Key 级 `expired_time/remain_quota` 是否被二开保留；③ 站点是否有套餐/订阅自定义端点（类似 SharedChat）；④ 余额差分推算消耗的可行性。

## 两站实测（2026-07-22）

用户提供实测渠道：渠道 1 = upstream-c.invalid（Sub2API），渠道 2 = upstream-d.invalid（NewAPI）。已探测公开端点。（✅ 鉴权取数已于 2026-07-23 完成，另加 upstream-e.invalid、upstream-f.invalid 共 4 站，见 [实测探测结果](./ISSUE-002-probe-results.md)。）

### 渠道 2：upstream-d.invalid（NewAPI 家族，`v1.0.0-rc.19`，"redacted-channel-19公益站"）

**站点开关（`GET /api/status`，无鉴权）**：`turnstile_check=false`（**无 WAF/盾，服务端可直采**）、`password_login_enabled=true`、`linuxdo_oauth=true`、`register_enabled=true`、`email_verification=true`、`quota_display_type=CNY`、`quota_per_unit=500000`。

**零凭证即可采**：`GET /api/pricing` 完全公开，返回 `model_ratio`、`completion_ratio`、`cache_ratio`、`group_ratio`（如 `Plus&team混池:2`）、`pricing_version`。**FR-010 价格/倍率采集无需任何账号**。

**需系统访问令牌才能采**（账号级）：`/api/user/self`（余额 `quota`）、`/api/token`（Key 列表：`remain_quota`、`used_quota`、`unlimited_quota`、`expired_time`、`group`、`model_limits`）、`/api/log/self/stat`（精确消耗）、`/api/user/self/groups`+`/api/group`（分组倍率）、`/api/user/models`。

**取 token 机制**（all-api-hub 源码）：NewAPI 有独立"系统访问令牌"概念。① 手动：用户在网页"个人设置"生成系统访问令牌；② 程序化：Cookie 登录态下 `GET /api/user/self` 返回 `access_token` 字段，为空则 `GET /api/user/token` 自动创建。调用时头部为 `Authorization: Bearer <token>` + `New-API-User: <user_id>`（all-api-hub 还向 `User-id` 等多个兼容头 fan-out）。

**有效期/可靠性**：系统访问令牌**长期有效**，除非用户手动重置，**无需刷新循环**，是服务端轮询的理想凭证。可靠性最高。

**需要用户提供**：系统访问令牌 + 数字 user_id（首选，不需要密码）；或账号密码由采集器登录后自动 mint。

### 渠道 1：upstream-c.invalid（Sub2API，`0.1.163`，"Subscription to API Conversion Platform"）

**站点开关（`GET /api/v1/settings/public`，无鉴权）**：`turnstile_enabled=false`（**无 WAF**）、`risk_control_enabled=false`（**无风控**）、`registration_enabled=true`、`totp_enabled=false`、`payment_enabled=true`、`backend_mode_enabled=false`。站点定位即"订阅转 API"，最可能暴露完整订阅元数据。

**需 JWT Bearer 才能采**：`/api/v1/auth/me`（余额 `balance`）、`/api/v1/keys`（`quota`、`quota_used`、`expires_at`、group `rate_multiplier`）、`/api/v1/groups/available`+`/rates`（实测无鉴权返回空）、`/api/v1/usage/stats`。**待认证后探测是否有订阅/套餐端点**（如 `/api/v1/subscriptions`），这是本站相对 NewAPI 的关键增量。

**取 token 机制**（all-api-hub 源码）：Sub2API 用 JWT。`access_token`（Bearer，短期，`expires_in`）+ `refresh_token`；到期前约 2 分钟用 `POST /api/v1/auth/refresh`（body `{refresh_token}`）换新。初始 pair 由登录获得，all-api-hub 从登录态浏览器捕获，自身不做密码登录。

**有效期/可靠性**：access_token **短期**，须维护刷新循环；refresh_token TTL 较长（DTO 见 `expires_in_days`），过期后需密码重登。可靠性中等——比 NewAPI 多一个刷新循环，但 molifang 无风控、无盾，工程可行。

**需要用户提供**：molifang 账号邮箱+密码（采集器登录并维护 JWT/refresh 循环，最可持续）；或登录后从浏览器捕获的 `access_token`+`refresh_token` 一对（refresh 过期后仍需重登）。

### 能否直接借用 all-api-hub

不能作为服务调用——它是浏览器扩展（WXT，跑在 Chrome/Firefox/Safari），客户端、AGPL-3.0。**服务端采集器需自研**，借鉴其：端点清单、DTO 字段、两套鉴权流程、兼容头 fan-out、最小间隔限速设计（不复制代码）。NewAPI 的服务端路径比扩展更干净（用手动系统令牌，跳过 Cookie 过盾环节）。

### 两站共同结论

两站当前均**关闭 Turnstile/风控，服务端可直接 HTTP 采集，无需浏览器自动化过盾**。风险：① 公益站可能随时开盾/改版，届时 NewAPI 走手动令牌、Sub2API 走手动捕获兜底；② 站点稳定性和端点契约不保证（版本各异）。凭证到位后即可跑一次实采，验证字段可得性并填写[验证记录表](../tech-selection/research/subscription-data-verification.md)。
