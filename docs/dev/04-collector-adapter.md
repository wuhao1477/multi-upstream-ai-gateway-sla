# 04 采集器契约（CollectorAdapter，v0.1 草案）

| 项目 | 内容 |
| --- | --- |
| 状态 | ✅ **v1.0 基线（2026-07-26 冻结）** —— 经 28 轮对抗性审查 + 2 轮开发视角走查 + PM 开工前裁决；变更须走版本记录 |
| 日期 | 2026-07-23 |
| 定位 | 采集侧（异步控制路径）契约。承接 [ISSUE-002 §2 适配器契约](../issues/ISSUE-002-collector-adapter-design.md#2-适配器契约)，落成 Go 接口签名 + 三家族实现要点 + 凭证生命周期状态机 + 写入 [02 数据模型](./02-data-model.md) 的目标表。**不涉及请求链路**（TTFT/接管/取消/隐藏重试属 `executor`/`ledger`，见 [01 架构](./01-architecture.md)）。 |
| 输入 | [PRD v1.4](../PRD.md)、[DECISIONS](../DECISIONS.md)、[ISSUE-001 运行时结论](../issues/ISSUE-001-tech-assumption-verification.md)、[ISSUE-002 采集适配器设计](../issues/ISSUE-002-collector-adapter-design.md)、[ISSUE-002 探测实测](../issues/ISSUE-002-probe-results.md)、[00 总览](./00-overview-and-milestones.md)、[01 架构](./01-architecture.md) |
| 覆盖 FR | FR-010/011/012/013/017/018、FR-020～032、FR-116（⏭ FR-033～039 订阅采集移入二期，[15 §1.2](./15-scope-and-preflight.md)） |
| 覆盖 AC | AC-17、AC-28、AC-29（⏭ AC-20～24 订阅制维持二期） |
| 里程碑 | M3 元数据采集（见 [00 §3](./00-overview-and-milestones.md#3-里程碑)） |

> **一条总原则（承接 ISSUE-002 前置结论）**：上游站点异构程度高，**必须按站型分流**。NewAPI/Sub2API 是通用开源项目，同族站点复用同一适配器但接入前必须先探测确认；ASXS 是闭源自建平台，**一站一适配器，不可复用、不作探测基准**。任何不支持的字段返回 `unsupported`，**不静默留空**（与上游对接层的能力声明原则一致，[03](./03-upstream-layer.md)）。

---

## 1. 接口签名（Go）

采集器是 `sla-core` 的旁路模块（同二进制子命令或独立容器，见 [01 §1](./01-architecture.md#1-组件拓扑)）。所有站型实现同一 `CollectorAdapter` 接口，并**显式声明能力**。

```go
package collector

import (
	"context"
	"time"
)

// ── 支持级别（对应 ISSUE-002 "支持/降级/不支持"）──
type SupportLevel string

const (
	Supported   SupportLevel = "supported"   // 有该对象且可自动采集
	Degraded    SupportLevel = "degraded"    // 部分可采 / 需人工补全 / 数据陈旧
	Unsupported SupportLevel = "unsupported" // 该站型无此对象（如 NewAPI 无订阅）
)

// ── 站型家族 ──
type Family string

const (
	FamilyNewAPI  Family = "newapi"
	FamilySub2API Family = "sub2api"
	FamilyASXS    Family = "asxs"
	FamilyUnknown Family = "unknown"
)

// Capability 即接口里除 detect/authenticate/capabilities 外的每个 fetch 能力
type Capability string

const (
	CapAccount            Capability = "account"
	CapKeys               Capability = "keys"
	CapGroups             Capability = "groups"
	CapSubscriptionQuotas Capability = "subscription_quotas"
	// ⏭ 一期**所有站型**一律返回 unsupported（订阅制整体移入二期，[15 §1.2](./15-scope-and-preflight.md)）。
	//    此前 Sub2API/ASXS 的 Capabilities() 标 supported 而 FetchSubscriptionQuotas 又返回
	//    ErrUnsupported —— 自相矛盾，且 M3 的 AC-28 要求「能力矩阵与实现一致」，照原文必挂（第 18 轮 [high]）。
	CapPricing            Capability = "pricing"
)

type CapabilityMap map[Capability]SupportLevel

// ── 采集来源元信息：每份数据都带，服务 FR-011 / FR-012 / FR-116 ──
type SourceMeta struct {
	Source     string        // "api" | "manual" | "derived"
	Endpoint   string        // 实际命中的端点，便于审计
	FetchedAt  time.Time     // 查询时间
	ValidUntil time.Time     // 过期时间；人工录入默认 FetchedAt+7d（FR-011）
	Stale      bool          // 是否已过期（**内存态判定**，不落库；库侧查 collector_snapshots_v 视图）→ 下游按"越旧越保守"降级
}

// ── 接口 ──
type CollectorAdapter interface {
	// 站型探测：公开、无鉴权，接入任何新站点的第一步（§2）
	Detect(ctx context.Context, baseURL string) (DetectResult, error)

	// 鉴权：把凭证换成会话句柄（含令牌、用户ID、到期时间等，§5 状态机）
	Authenticate(ctx context.Context, cred Credential) (Session, error)

	// 账号级：余额、已用、用户ID（FR-020/024/026）
	FetchAccount(ctx context.Context, s Session) (Account, error)

	// Key 级：额度、有效期、限流、模型权限（FR-003/028/031）
	FetchKeys(ctx context.Context, s Session) ([]Key, error)

	// 分组/倍率：分组倍率、订阅类型、周期上限、高峰倍率（FR-010/033）
	FetchGroups(ctx context.Context, s Session) ([]Group, error)

	// 订阅周期额度（FR-033～039）。⏭ **一期不实现**（[15 §1.2](./15-scope-and-preflight.md)）：
	// 一期所有站型统一返回 ErrUnsupported；二期按站型实现。
	FetchSubscriptionQuotas(ctx context.Context, s Session) ([]SubscriptionQuota, error)

	// 模型价格：输入/输出/缓存倍率（FR-010/012/013）
	FetchPricing(ctx context.Context, s Session) (Pricing, error)

	// 静态能力声明：调度前即可知道该站能采什么
	Capabilities() CapabilityMap
}

// 不支持的 fetch 统一返回该哨兵错误；调用方据此登记 Unsupported，不留空
var ErrUnsupported = errors.New("collector: capability unsupported by this family")

// ── 接口引用但此前未定义的类型（第 29 轮 [P0] 补齐）──
// 开发按原文档写到 Authenticate() 就卡住：Credential/Session 只有名字没有结构。

// Credential：运维登记的采集凭证，落 collector_credentials（[02 §7](./02-data-model.md)，一期明文）
type Credential struct {
    ChannelID  int64
    BaseURL    string
    Kind       string // 'password' | 'access_token' | 'api_key'
    Username   string // Kind='password' 时用
    Password   string
    Token      string // Kind='access_token'/'api_key' 时用
    ExtraHeaders map[string]string // 如 NewAPI 二开的 New-API-User
}

// Session：Authenticate 的产出，含续期所需的一切。**内存态，不落库**（含明文令牌）
type Session struct {
    Family      Family
    BaseURL     string
    AccessToken string
    RefreshToken string    // 仅 Sub2API 有；ASXS 无 refresh，到期须账密重登（§5）
    UserID      string     // NewAPI 的数字用户 ID，用于 New-API-User 头
    ExpiresAt   time.Time  // 令牌到期；提前 ExpiryGrace 续期
    Headers     map[string]string // 每次请求都要带的固定头
}

// Pricing：模型价格表。**入库不做单位归一**（保留上游口径便于对账），
// 缩放只发生在算成本那一处（[02 §3](./02-data-model.md) 成本公式）
type Pricing struct {
    SourceMeta
    Items []PriceItem
}
type PriceItem struct {
    ModelName   string
    InputPrice  float64
    OutputPrice float64
    CachePrice  float64
    BillingUnit string // 'per_1m_token' | 'per_1k_token' | 'per_token'，**必须回填，不得猜**
    GroupRatio  float64 // 分组倍率；无分组概念时为 1
}

// RateLimit：Key 级限流，回填 bindings.rpm_limit / concurrency_limit
// （[05 §4.3](./05-scheduling-and-operations.md)：未登记容量 = 不启用保留）
type RateLimit struct {
    RPM         *int // nil = 该站不暴露，**不可当成 0 或无限**
    Concurrency *int
}

// ManualReserve：无法采集时运维手填的保守余额下限（§7 降级路径）
type ManualReserve struct {
    AmountUSD  float64
    SetBy      string
    SetAt      time.Time
    Note       string
}
```

**分页、限流与重试（三家族统一约定）**：

| 项 | 约定 |
| --- | --- |
| 分页 | 统一 `page`(从 1)/`page_size`(默认 100)；适配器内部循环取完再返回，**不把分页暴露给调用方** |
| 单站并发 | 1（顺序采集）—— 采集是后台任务，没有并发必要，反而容易触发上游风控 |
| 站内请求间隔 | `collector_request_interval_ms`，默认 200ms |
| 失败重试 | 指数退避 1s/2s/4s，最多 3 次；仍失败 → 该站本轮放弃，`collector_credentials.status='error'` + P2 告警，**不影响其它站** |
| 429/403 | 立即停止本轮该站采集并退避到下一周期（不重试），避免把凭证打死 |
| 超时 | 单请求 10s，整站一轮 120s |

**返回结构（均内嵌 `SourceMeta`，金额统一归一为数值美元 —— FR-018 一期 1:1，AC-17）**：

```go
type DetectResult struct {
	Family      Family
	Version     string // 如 NewAPI "v1.0.0-rc.19"、Sub2API "0.1.163"
	NoShield    bool   // turnstile 是否关闭（§6 无盾确认）
	UserIDHeader string // NewAPI 二开 fan-out 命中的头名，其余家族为空
}

type Account struct {
	UserID     string
	BalanceUSD float64 // 已归一为美元
	UsedUSD    float64
	Meta       SourceMeta
}

type Key struct {
	KeyRef         string    // 脱敏引用，不存明文（FR-094）
	RemainQuotaUSD float64
	UsedQuotaUSD   float64
	Unlimited      bool
	ExpiredAt      *time.Time
	ModelLimits    []string  // 模型权限（NewAPI model_limits / Sub2API 分组）
	RateLimit      RateLimit // RPM/并发/窗口限流（FR-028）
	GroupRef       string
	Meta           SourceMeta
}

type Group struct {
	GroupRef        string
	RateMultiplier  float64 // 分组倍率
	PeakEnabled     bool
	PeakMultiplier  float64
	PeakStart, PeakEnd string
	SubscriptionType string
	Platform        string // openai / anthropic 等
	RPMLimit        int
	IsExclusive     bool
	Meta            SourceMeta
}

type SubscriptionQuota struct {
	OwnerKey       string    // 共享额度归集键：Sub2API 为 (user,group)，ASXS 为 (user,plan)（FR-035）
	PlanRef        string
	StartsAt, ExpiresAt time.Time
	Status         string    // active / expired / suspended
	WindowMode     string    // fixed（固定窗口重置）
	Period         string    // daily / weekly / monthly
	LimitUSD       float64   // 周期包含额度
	UsedUSD        float64
	LeftUSD        float64
	ResetAt        *time.Time
	FixedFeeUSD    float64   // 固定费用（算用满倍率，FR-057）
	DurationDays   int       // 有效期天数（算用满倍率）
	RateMultiplier float64
	PrimarySource   string   // 额度来源优先级（ASXS primarySource，FR-033/8.5）
	SecondarySource string
	ManualResetPolicy *ManualReset // 可主动重置额度（ASXS dailyReset，FR-036 外生变量）
	Overage        *OverageRule // Sub2API 系恒为 nil（无超额）
	Meta           SourceMeta
}
```

---

## 2. 站型探测（`Detect`，公开无鉴权，按顺序命中即停）

接入任何新站点的第一步。全部公开端点、零成本，可对 ~20 个上游批量跑一次自动分桶（AC-28）。

| 顺序 | 探测请求 | 命中特征 | 归类 |
| --- | --- | --- | --- |
| 1 | `GET /api/status` | 返回含 `quota_per_unit`、`turnstile_check`、`checkin_enabled` | **NewAPI 系** → 复用 `NewApiAdapter` |
| 2 | `GET /api/v1/settings/public` | 返回含 `site_name`、`turnstile_enabled`、envelope `{code,message,data}` | **Sub2API 系** → 复用 `Sub2ApiAdapter` |
| 3 | `GET /api/public/site-config` | 200 且 JWT `iss` 为 `ampmanager` | **ASXS 系** → 专属 `AsxsAdapter` |
| 4 | 全未命中 | —— | **未知家族** → §7 接入流程，新建专属适配器 |

**二开兼容要点**：NewAPI 系即使命中顺序 1，用户 ID 头名仍可能不同，探测阶段不确定，`Detect` 只归族；`UserIDHeader` 由 `Authenticate` 阶段 fan-out 试探（§3.1）。`quota_per_unit` 也逐站从 `/api/status` 读取，**不写死**（upstream-d.invalid 为 500000）。

---

## 3. 三家族实现要点与字段映射

### 3.1 NewAPI 系（upstream-d.invalid 已验证）— 无订阅对象

| 项 | 结论 |
| --- | --- |
| 鉴权 | `Authorization: <token>` 或 `Bearer <token>` **均可**；**必须同时带 `New-API-User: <数字用户ID>`**，只带 Cookie 会 401 |
| 二开头名 fan-out | 首次鉴权对多头名逐一试探命中：`New-API-User` / `Veloera-User` / `X-Api-User` / `voapi-user` / `User-id` / `Rix-Api-User` / `neo-api-user`，命中后记入 `Session.UserIDHeader` |
| 额度换算 | `金额(USD) = quota / quota_per_unit`（upstream-d.invalid `quota_per_unit=500000`，已实测与页面一致）。换算在适配器内完成（FR-018、AC-17） |
| 订阅 | ❌ 无订阅对象。`FetchSubscriptionQuotas` 返回 `ErrUnsupported` |
| 签到额度 | `/api/status.checkin_enabled=true`（upstream-d.invalid 本月签到 ¥160）。**一期不建模为额度来源**（FR-034 删签到句）；仅可作为 note 记录，接受该类站点额度预测系统性偏低 |

**字段映射：**

| fetch | 端点 | 源字段 | → 归一/落库字段 | FR |
| --- | --- | --- | --- | --- |
| `FetchAccount` | `/api/user/self` | `quota`、`used_quota`、`request_count`、`group` | `BalanceUSD=quota/qpu`、`UsedUSD=used_quota/qpu`、`UserID` | FR-020/024 |
| `FetchKeys` | `/api/token` | `expired_time`、`remain_quota`、`unlimited_quota`、`used_quota`、`model_limits`、`model_limits_enabled`、`allow_ips`、`group` | `ExpiredAt`、`RemainQuotaUSD`、`Unlimited`、`ModelLimits`、`GroupRef` | FR-003/028/031 |
| `FetchGroups` | 由 `/api/pricing.group_ratio` 派生 | `group_ratio` | `RateMultiplier` | FR-010 |
| `FetchPricing` | `/api/pricing`（**公开**） | `model_ratio`、`group_ratio`、`cache_ratio`、`completion_ratio` | 价格版本（不可覆盖，FR-012） | FR-010/012/013/017 |
| `FetchSubscriptionQuotas` | —— | —— | `ErrUnsupported` | FR-034 |

> ⚠️ **`/api/user/token` 是"重新生成"而非"读取"** —— 实测每次调用都返回新令牌并**立即作废旧令牌**。本版本 `/api/user/self` 不回显 `access_token`，无法惰性"读不到再建"。采集器**只在初始化时调用一次并持久化**，运行时**禁止**重新生成（§5 状态机 N-1）。

**Capabilities：**
```
{account: supported, keys: supported, groups: supported, pricing: supported, subscription_quotas: unsupported}
```

### 3.2 Sub2API 系（molifang + hyhawang 实测 + 源码级 ent schema 解析）

| 项 | 结论 |
| --- | --- |
| 鉴权 | `Authorization: Bearer <JWT>`，JWT 有效期 **24 小时**（`exp-iat=86400`） |
| 凭证存储 | localStorage：`auth_token`、`refresh_token`、`token_expires_at`、`auth_user` |
| 续期 | `POST /api/v1/auth/refresh`，body `{refresh_token}` → 返回新 access + refresh + `expires_in`。**到期前 120s 主动刷新**（`SUB2API_TOKEN_REFRESH_BUFFER_MS`），**按账号加互斥锁串行刷新**（§5） |
| 无超额 | 源码 ent schema **无 overage 字段**，用尽即在窗口内阻断至重置。`Overage` 恒为 nil（FR-033 该项对本族"不适用"） |
| 固定窗口重置 | `ResetDaily/Weekly/MonthlyUsage` + `window_start`，对应 API `*_window_resets_at`（FR-036 可直接用于到期未用预测） |

**运行时端点字段映射：**

| fetch | 端点 | 源字段 | FR |
| --- | --- | --- | --- |
| `FetchAccount` | `/api/v1/auth/me` | 用户ID、邮箱、角色 | FR-020 |
| `FetchKeys` | `/api/v1/keys` | `quota`、`quota_used`、`expires_at`、`rate_limit_5h/1d/7d`、`usage_5h/1d/7d`、`window_*_start`、`current_concurrency` | FR-020～032 |
| `FetchGroups` | `/api/v1/groups/available` | `subscription_type`、`rate_multiplier`、`daily/weekly/monthly_limit_usd`、`peak_rate_enabled/peak_start/peak_end/peak_rate_multiplier`、`rpm_limit`、`is_exclusive`、`fallback_group_id` | FR-010/033 |
| `FetchSubscriptionQuotas` | `/api/v1/user/platform-quotas`（+ `/api/v1/subscriptions[/active]`） | `platform`、`daily/weekly/monthly_limit_usd`、`*_usage_usd`、`*_window_resets_at` | FR-033～039 |

**源码级订阅模型（sub2api `63cef60`，ent schema 为准 —— 比空账号更完整）** — 4 张核心表逐字段确认 FR-033/034：

| 源码表 | 字段 | → `SubscriptionQuota` / FR |
| --- | --- | --- |
| `subscription_plans`（可售套餐） | `name`、`price`、`original_price`、`currency`、`validity_days`、`validity_unit`、`features`、`for_sale`、`group_id` | `FixedFeeUSD=price`、`DurationDays=validity_days×unit`、`PlanRef`；支持模型/倍率经 `group_id` 关联（FR-033） | ⏭ **二期** |
| `user_subscriptions`（已购实例） | `user_id`、`group_id`、`starts_at`、`expires_at`、`status`(active/expired/suspended)、`daily/weekly/monthly_window_start`、`daily/weekly/monthly_usage_usd`、`assigned_by`、`notes` | `StartsAt/ExpiresAt/Status/UsedUSD/ResetAt`（FR-034） | ⏭ **二期** |
| `group`（额度与倍率载体） | `rate_multiplier`、`peak_*`、`subscription_type`、`daily/weekly/monthly_limit_usd`、`rpm_limit`、`platform`、`is_exclusive` | `LimitUSD`（周期上限）、`RateMultiplier`、高峰倍率、`WindowMode=fixed`（FR-033/036） |
| `user_platform_quota`（平台额度视图） | `platform`、`daily/weekly/monthly_limit_usd` + `_usage_usd` + `_window_start` | 平台级周期额度 + 已用 + 重置（FR-034） |

> **共享额度（FR-035、AC-22）确证**：订阅用量按 `(user_id, group_id)` 聚合（账务入口 `UpdateSubscriptionUsage(userID, groupID, cost)`）。同一用户同一 group 下**多个 Key 天然共享同一份订阅额度**。`SubscriptionQuota.OwnerKey` 必须设为 `(user,group)` 归集，**不能按 Key 归集**，否则重复计算剩余额度。

**Capabilities：**
```
{account: supported, keys: supported, groups: supported, pricing: degraded, subscription_quotas: **unsupported（一期）**}
```
（价格倍率经 `/api/v1/groups/available.rate_multiplier` 与分组耦合，非独立价格表，标 `degraded`。）

### 3.3 ASXS Codex 系（upstream-f.invalid 已验证）— 订阅数据最完整，专属不可复用

| 项 | 结论 |
| --- | --- |
| 底层产品 | JWT `iss:"ampmanager"`、`aud:"ampmanager-users"` —— 闭源 AMP Manager，**非任何开源项目** |
| 命名空间 | `/api/me/*`（用户数据，Bearer JWT）、`/api/public/*`（公开）、`/api/manage/auth/*`（登录）、`/api/auth/*`+`/api/user/*`（API-Key 代理，与仪表盘 JWT 不通用） |
| 鉴权 | JWT `Authorization: Bearer`，localStorage 键 `token`；**有效期 168 小时（7 天）**，无 cookie |
| 续期 | ❌ **无任何静默续期**（无 refresh_token、无 refresh claim、`/api/me/refresh` 等全 404、页面加载无 auth 调用）。唯一续法：`POST /api/manage/auth/login` **账号密码重登**换新 7 天 JWT（§5） |
| 额度单位 | micros（`limitMicros:90000000`=$90）；`金额(USD)=micros/1e6` |
| 专属性 | ⚠️ **本适配器专属，不可复用、不作探测基准**（命名空间/micros/JWT claim 均与 NewAPI/Sub2API 零重叠） |

**字段映射（`/api/me/billing/state` —— FR-033 要求字段几乎全覆盖）：**

| fetch | 端点 | 源字段 | → 字段 / FR |
| --- | --- | --- | --- |
| `FetchAccount` | `/api/me/balance`、`/api/me/profile` | `balanceUsd`（字符串）、`limitMicros` | `BalanceUSD`（FR-020） |
| `FetchSubscriptionQuotas` | `/api/me/billing/state` | `planId`/`planName`、`startsAt`、`expiresAt`、`limits[].limitType`+`windowMode`、`limits[].limitMicros`、`limits[].fixedResetTime`、`status`、`usedMicros`/`leftMicros`、`remainingDays` | `PlanRef/StartsAt/ExpiresAt/Period/WindowMode=fixed/LimitUSD/ResetAt/Status/UsedUSD/LeftUSD`（FR-033～039） |
| `FetchSubscriptionQuotas`（固定费用） | `/api/me/purchase/products`（16 套餐） | `priceCnyCent`、`durationDays`、`subscriptionPlanName`、`renewAllowed`、`renewalRule` | `FixedFeeUSD`（priceCnyCent 一期 1:1 归一美元）、`DurationDays` —— **双倍率用满倍率的全部输入**（FR-057/058、AC-24） |

**两个关键额外字段（原 PRD 未预见，已并入 FR-033）：**

- `primarySource:"subscription"` / `secondarySource:"balance"` → `PrimarySource/SecondarySource`，即平台**自身**的额度消耗优先级。不读会错判订阅额度是否真被消耗（FR-033、8.5）。
- `dailyReset`（`usageThresholdPercent:90%`、`dailyLimit:4` 次/日）→ `ManualResetPolicy`，可主动争取的额外额度，是 FR-036 到期未用预测的**外生变量**。

> 实测当下即一个真实"额度浪费"案例：该订阅 2 天后到期、当日 $90 额度 `usedMicros=0` → 可直接作 FR-036/FR-039 验收用例（AC-20/AC-21）。

**Capabilities：**
```
{account: supported, keys: unsupported, groups: unsupported, pricing: degraded, subscription_quotas: **unsupported（一期）**}
```
（ASXS 无独立 Key/分组管理视图；价格并入套餐 products，标 `degraded`。）

### 3.4 三家族能力矩阵总表

| Capability | NewAPI | Sub2API | ASXS |
| --- | --- | --- | --- |
| `account` | supported | supported | supported |
| `keys` | supported | supported | unsupported |
| `groups` | supported | supported | unsupported |
| `subscription_quotas` | **unsupported** | **unsupported（一期）** | **unsupported（一期）** |  ⏭ 订阅制整体移入二期（[15 §1.2](./15-scope-and-preflight.md)）；`Capabilities()` 的声明必须与 `FetchSubscriptionQuotas` 返回 `ErrUnsupported` 一致，否则 [AC-28](./14-acceptance-matrix.md) 判不通过 |
| `pricing` | supported（公开） | degraded | degraded |
| 令牌与续期 | 系统访问令牌，长期，初始化一次生成 | JWT 24h + refresh 无密码续期 | JWT 7d，无 refresh，账密重登 |
| 额度单位 | `quota/quota_per_unit` | USD 浮点 | `micros/1e6` |
| 共享额度归集键 | Key 独立 | `(user,group)` | `(user,plan)` |
| 超额计费 | N/A | **无超额**（用尽即阻断） | 有 `renewAllowed`/多订阅 |

---

## 4. 写入 [02 数据模型](./02-data-model.md) 的目标表

[02 数据模型](./02-data-model.md) 已定义下列表，本节给出采集器 → 02 的**写入映射**（表名对齐 02 实际结构）。采集器**只写这些表**，请求路径不读原始表而读内存快照（[01 §5](./01-architecture.md#5-数据面控制面切分)）。

| 02 实际表（[02](./02-data-model.md)） | 写入方法 | 关键列 | FR |
| --- | --- | --- | --- |
| `channels`/`upstream_accounts`/`upstream_keys`/`models` | Detect + Fetch* | 资源身份登记，不以渠道名代身份；`site_family`、`external_user_id` | FR-002 |
| `price_versions` | `FetchPricing` | 币种、计费单位、来源、查询/生效时间；**不可覆盖版本** | FR-012/013 |
| `price_change_log` | `FetchPricing` **同一事务** | 见下方「价格变更留痕」 | FR-014/017 |
| `balance_signals` | `FetchAccount` | `last_confirmed_balance`、`balance_state`、`conservative_floor`、`quota_status` | FR-020/024/026 |
| `upstream_keys` + `collector_snapshots` | `FetchKeys` | key 级 `remain_quota/expired_time/model_limits` + 限流快照（payload） | FR-021/028/031 |
| `subscription_plans`（含 `rate_multiplier`/`peak_*`） | `FetchGroups` + `FetchSubscriptionQuotas`（套餐维度） | 分组/高峰倍率、固定费用、有效期、支持模型、续订状态、**`usable_multiplier`/`actual_multiplier`** 双倍率 | FR-010/033、参数14 | ⏭ **二期** |
| `user_subscriptions` + `subscription_quota_windows` | `FetchSubscriptionQuotas`（实例维度） | `(ext_user_id, group_id)` 共享归集、周期额度/已用/剩余/重置、`primary/secondary_source`、`active_reset_*`、`overage_rule(no_overage_block for sub2api)` | FR-034/035/036、8.5 | ⏭ **二期** |
| `collector_credentials` | `Authenticate` 副产物 | 令牌/refresh/账密（一期明文，FR-113），脱敏引用入日志（FR-094） | FR-113 |
| `collector_snapshots`（内嵌 `data_source/fetched_at/valid_until`） | 所有 Fetch* | source、endpoint、fetched_at、valid_until（人工 +7d）；**陈旧性不落列**，查 `collector_snapshots_v.is_stale` 视图（[02 §7](./02-data-model.md)） | FR-011 |

**价格变更留痕**（第 40 轮补：`price_change_log` 的 `from_version_id`/`to_version_id`/`direction`
建好后**零引用**——没有任何写入方，FR-014「降价需再次确认」与 FR-017「价格变化记录和影响范围」
双双落空；`GET /admin/prices/changes` 也就无数据可查）：

`FetchPricing` 每次插入新 `price_versions` 行时，**同一事务**内比对上一版并留痕：

| 情形 | `direction` | `confirmed` | 后续动作 |
| --- | --- | --- | --- |
| 新价 > 旧价 | `increase` | `true` | 直接生效（涨价照单全收，不需确认） |
| 新价 < 旧价 | `decrease` | **`false`** | ⚠️ **不直接采信**（FR-014/AC-03）：降价可能是采集页面改版误读。生效但标记待验证，由一次真实扣费与预估一致后置 `true`（[05 §4.4](./05-scheduling-and-operations.md) 计费对账），或人工 `POST /admin/prices/changes/{id}/confirm` |
| 价格未变但 `queried_at` 刷新 | `confirmed` | `true` | 仅刷新新鲜度，不改判 |
| 采集失败/超期未采到 | `stale` | `false` | 不插 `price_versions`，只记一条 `stale` 留痕 + P3 告警（`category='data_stale'`） |

- **比对口径**：`(input_price, output_price, cache_price, billing_unit)` 四项任一不同即算变更；
  `billing_unit` 变了要**先归一再比大小**，否则 `per_1k_token` 换成 `per_1m_token` 会被误判成暴涨 1000 倍。
- **首次采到该 `(channel, model)`**：`from_version_id` 为 NULL、`direction='confirmed'`，不算变更。
- **`decrease` 未确认期间照常使用新价**：保守方向是"按更贵的算"，而低价会让 selector 更倾向选它——
  故未确认的降价**不参与"低价优选"排序**（与 §1.1 序 6 价格陈旧的处置一致），只作保底。

> **ASXS 归集键差异**：02 的 `user_subscriptions` 用 `(ext_user_id, group_id)` 表达共享额度。ASXS 无 group 概念，其 `group_id` 列填**套餐标识**（ASXS 按 `(user, plan)` 聚合，见 §3.4）；采集器负责把家族差异映射到统一列。

> **双倍率落库分工**：`subscription_plan.用满倍率 = 固定费用÷周期额度×分组倍率`（调度排序，FR-057），`subscription_instance.实际倍率 = 固定费用÷实际消耗×倍率`（账务报表，FR-058）。采集器只提供两者的**原料**（`FixedFeeUSD`、`DurationDays`、`LimitUSD`、`UsedUSD`、`RateMultiplier`），倍率计算由 `steward`/账本完成，不在采集器内。

---

## 5. 凭证生命周期状态机（核心风险区）

三家族**都有"凭证互斥作废"风险**，这是采集器最容易出事的地方。凭证操作按站型分三套状态机，均以 `collector_credential` 持久化为唯一真相。

**两列的持久化落点**（第 40 轮补：`collector_credentials.user_id_header_name` 与 `refresh_lock_key`
建好后**零引用** —— §3.1 的 fan-out 结果只写进内存 `Session.UserIDHeader`，进程一重启就得重新试探
七个头名；"按账号加互斥锁"也只是散文，锁键从哪来没说）：

| 列 | 谁写 | 谁读 |
| --- | --- | --- |
| `user_id_header_name` | `Authenticate` 完成 fan-out 后**立即持久化**命中的头名（§3.1 七选一） | 后续每次采集直接取用，**不再 fan-out**；命中失效（401）时清空该列并重试试探 |
| `refresh_lock_key` | 凭证登记时按账号生成，**同一上游账号的多条凭证共用同一值**（如 `refresh:<site_family>:<external_user_id>`） | Sub2API 刷新前 `pg_advisory_xact_lock(hashtext(refresh_lock_key))` 串行化——并发刷新会互相作废（§5.2） |

- **为什么必须落库而非只在内存**：fan-out 是**七次带凭证的试探请求**，重启就重来一遍，
  既慢又平白给上游制造异常鉴权记录；而互斥锁若只在进程内，**多实例部署时完全失效**——
  [06](./06-deployment-and-operations.md) 的 compose 本来就可能跑多个采集器副本。

### 5.1 NewAPI 系统访问令牌（长期，互斥作废）

```
[未接入] --初始化: 登录取令牌; 若 self 无 access_token 则 /api/user/token 生成一次--> [持有令牌]
[持有令牌] --正常采集 (Authorization + New-API-User 头 fan-out)--> [持有令牌]
[持有令牌] --401/令牌被后台重置--> [失效] --告警, 人工介入重新登录取新令牌--> [持有令牌]

不变式 N-1（硬约束）: 运行时禁止再次调用 /api/user/token —— 每次调用作废旧令牌，会踢掉正在使用的令牌。
建议: 用专用采集账号建令牌, 不与日常混用。
```

### 5.2 Sub2API JWT（24h + refresh，按账号互斥锁串行刷新）

四层续期策略（借鉴 all-api-hub），**按账号加互斥锁**，刷新结果立即持久化：

| 层 | 触发 | 动作 | 服务端可移植 |
| --- | --- | --- | --- |
| 1 主动刷新 | JWT 到期前 **120s** | `POST /api/v1/auth/refresh {refresh_token}` 换新 access+refresh | ✅ 纯 HTTP |
| 2 被动刷新 | 请求 401 | 用 refresh_token 再试一次 | ✅ 纯 HTTP |
| 3 重同步 | refresh_token 也失效 | 从浏览器会话读新 JWT | ❌ **不可移植**；服务端等价：保存登录 Cookie 或账密重登换 JWT |
| 4 放弃 | 全失败 | 抛"需要重新登录"错误 + 告警 | —— |

```
[持有 access+refresh] --到期前120s / 401--> [刷新中(持账号锁)] --成功--> [持有 access+refresh(已轮换)]
[刷新中] --refresh 失效--> [需重登] --账密/Cookie 重登--> [持有 access+refresh]

不变式 S-1（硬约束）: refresh 会轮换 refresh_token, 并发刷新互相作废 → 同一账号刷新必须串行(互斥锁), 刷新结果先持久化再释放锁。
设备绑定: 已确认 Sub2API 未开启 bnd 绑定, 服务端换 IP/UA 续期不被拒（风险解除）。
```

### 5.3 ASXS JWT（7d，无 refresh，账密重登）

```
[持有 JWT] --正常采集 (Authorization: Bearer)--> [持有 JWT]
[持有 JWT] --剩余有效期 < 阈值(建议 <1 天)--> [重登中] --POST /api/manage/auth/login {username,password}--> [持有新 7d JWT]
[持有 JWT] --401--> [重登中]

特性: 7 天有效期 → 重登频率极低(约每周 1 次), 风控压力小。
约束: 必须持有账号密码才能续期（闭源平台固有约束, 无 refresh 路径）；账密一期明文存储(FR-113)。
```

---

## 6. 通用防护

| 项 | 要求 | 来源 |
| --- | --- | --- |
| **限速** | 同站点请求强制最小间隔（参考 all-api-hub `minIntervalLimiter`），避免触发风控；查询频率默认见 PRD §11（价格 6h / 余额校对 5～15min / 订阅 1h），可配置 | FR-116、参数10 |
| **无盾确认** | 接入前必须确认 `turnstile_check:false`（NewAPI）/ `turnstile_enabled:false`（Sub2API）。**开盾站点服务端采集不可行**，须转人工录入。已实测 upstream-d.invalid、molifang 均无盾 | ISSUE-002 §5 |
| **凭证互斥作废** | 见 §5 三套状态机：NewAPI 令牌只生成一次、Sub2API 按账号串行刷新、ASXS 账密重登 | ISSUE-002 §4 |
| **降级一致性** | 任何字段采集失败一律落 FR-011：人工录入 + 数据来源标注 + **7 天有效期** + 过期按"订阅数据未知"降级（订阅状态机进"数据未知"，PRD §9.3）。**不允许用陈旧数据驱动订阅倾斜** | FR-011、AC-28 |
| **余额信号输入** | 采集器提供"账户/Key 余量是否归零"的判据，供 `steward` 的余额不足信号自适应识别框架（错误文案正则 + 失败信号 + 余量归零）判定资源置"耗尽" | FR-027、AC-29 |
| **额度归一** | 三家族 `quota/quota_per_unit`、USD 浮点、`micros/1e6` 一律在适配器内归一为数值美元，一期各币种 1:1（币种仅名称） | FR-018、AC-17 |

---

## 7. 未知家族接入流程

1. `Detect` 依次比对 `/api/status`、`/api/v1/settings/public`、`/api/public/site-config`，全未命中 → `FamilyUnknown`。
2. 抓取前端实际调用的接口（`performance.getEntriesByType('resource')` 思路取样），人工确认字段语义后写专属适配器。
3. 确无接口 → 按 FR-011 人工录入，标注来源、7 天有效期，过期按"订阅数据未知"降级。

---

## 8. 与其他篇章的边界

- **不含**请求链路能力：内容感知 TTFT、动态期限接管、mid-stream 取消传播、`errorMessage` 归并对账、同资源隐藏重试补算 —— 均属 `executor`/`ledger`（[01 §2](./01-architecture.md#2-sla-core-内部模块)、[00 硬约束 4/5/7](./00-overview-and-milestones.md#2-硬约束清单开发期不可违反)），本篇不涉及。
- **依赖** 02 定型上表结构；**被依赖** 于 `steward`（余额信号、订阅倾斜三道闸）与 `selector`（配额状态 unknown 默认排除 FR-118 —— 但采集器只负责采回状态，过滤在 selector 做）。
- 采集器是异步控制路径，任何抖动不阻塞同步决策；数据经后台快照供请求路径只读（[01 §5](./01-architecture.md#5-数据面控制面切分)）。
