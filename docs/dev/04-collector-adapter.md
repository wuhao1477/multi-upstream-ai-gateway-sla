# 04 采集器契约（CollectorAdapter，v0.1 草案）

| 项目 | 内容 |
| --- | --- |
| 状态 | ✅ **v1.0 基线（2026-07-26 冻结）** —— 经 42 轮对抗性审查（含 5 轮开发视角）+ PM 开工前裁决；变更须走版本记录 |
| 日期 | 2026-07-23 |
| 定位 | 采集侧（异步控制路径）契约。承接 [ISSUE-002 §2 适配器契约](../issues/ISSUE-002-collector-adapter-design.md#2-适配器契约)，落成 Go 接口签名 + 逐家族实现要点 + 凭证生命周期状态机 + 写入 [02 数据模型](./02-data-model.md) 的目标表。**不涉及请求链路**（TTFT/接管/取消/隐藏重试属 `executor`/`ledger`，见 [01 架构](./01-architecture.md)）。 |
| 输入 | [PRD v1.4](../PRD.md)、[DECISIONS](../DECISIONS.md)、[ISSUE-001 运行时结论](../issues/ISSUE-001-tech-assumption-verification.md)、[ISSUE-002 采集适配器设计](../issues/ISSUE-002-collector-adapter-design.md)、[ISSUE-002 探测实测](../issues/ISSUE-002-probe-results.md)、[00 总览](./00-overview-and-milestones.md)、[01 架构](./01-architecture.md) |
| 覆盖 FR | FR-010/011/012/013/017/018、FR-020～032、FR-116（⏭ FR-033～039 订阅采集移入二期，[15 §1.2](./15-scope-and-preflight.md)） |
| 覆盖 AC | AC-17、AC-28、AC-29（⏭ AC-20～24 订阅制维持二期） |
| 里程碑 | M3 元数据采集（见 [00 §3](./00-overview-and-milestones.md#3-里程碑)） |

> **一条总原则（承接 ISSUE-002 前置结论）**：上游站点异构程度高，**必须按站型分流**。NewAPI/Sub2API 是通用开源项目，同族站点复用同一适配器但接入前必须先探测确认；**全自研站**（闭源面板、自建平台）**一站一适配器，不可复用、不作探测基准** —— 接入路径见 §7bis「全自研站怎么接」。任何不支持的字段返回 `unsupported`，**不静默留空**（与上游对接层的能力声明原则一致，[03](./03-upstream-layer.md)）。

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
	FamilyUnknown Family = "unknown"
	// 加一族的落点只有两处：这里一个常量 + 一份 Registration（§7bis）
)

// Capability 即接口里除 detect/authenticate/capabilities 外的每个 fetch 能力
type Capability string

const (
	CapAccount            Capability = "account"
	CapKeys               Capability = "keys"
	CapGroups             Capability = "groups"
	CapSubscriptionQuotas Capability = "subscription_quotas"
	// ⏭ 一期**所有站型**一律返回 unsupported（订阅制整体移入二期，[15 §1.2](./15-scope-and-preflight.md)）。
	//    此前有站型的 Capabilities() 标 supported 而 FetchSubscriptionQuotas 又返回
	//    ErrUnsupported —— 自相矛盾，且 M3 的 AC-28 要求「能力矩阵与实现一致」，照原文必挂（第 18 轮 [high]）。
	CapPricing            Capability = "pricing"
	CapModelCatalog       Capability = "model_catalog"   // FR-126（P1 新增）
)

type CapabilityMap map[Capability]SupportLevel

// ── 采集来源元信息：每份数据都带，服务 FR-011 / FR-012 / FR-116 ──
type SourceMeta struct {
	Source     string        // "api" | "manual" | "derived"
	Endpoint   string        // 实际命中的端点，便于审计
	FetchedAt  time.Time     // 查询时间
	ValidUntil time.Time     // 过期时间；人工录入默认 FetchedAt+7d（FR-011）
	Stale      bool          // 是否已过期（**内存态判定**，不落库；库侧查 collector_snapshots_v 视图）→ 下游按"越旧越保守"降级
	// ── Degraded 能力的实际结果（§3.4bis，第 45 轮补）──
	// 声明 degraded 的能力照常返回数据与 nil error，用这两个字段说明"缺了什么"，
	// 供 sync 的 item.note 与 inventory 的"待人工补录"计数使用（FR-011）。
	Degraded      bool     // 本次结果是否不完整
	MissingFields []string // 缺失的字段名，如 ["input_price","output_price"]
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

	// 渠道可用模型目录（FR-126，**P1 新增**）：上游声称可用的**全部**模型。
	// 与 FetchPricing 的区别：Pricing 产出权威价格版本（FR-012 不可覆盖），
	// 本方法产出"上游有哪些模型"的清单——两者可能来自同一端点，但落库目标不同
	// （前者 price_versions，后者 channel_model_catalog）。
	FetchModelCatalog(ctx context.Context, s Session) ([]CatalogModel, error)

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
    RefreshToken string    // 仅有 refresh 路径的站型有；无令牌端点的站型到期须账密重登（§5）
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

**分页、限流与重试（全家族统一约定）**：

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
	GroupRef        string   // → channel_groups.group_ref（P1 落库）
	RateMultiplier  float64  // 分组倍率 → channel_groups.rate_multiplier（P1 落库）
	AvailableModels []string // 该分组可获取的模型（FR-124）→ group_models.model_name（P1 落库）
	RPMLimit        int      // → 与 Key 级限流一并登记（FR-127）
	// ── 以下字段 P1 采集但**不落结构化列**，只进 collector_snapshots.payload ──
	// 理由（[ISSUE-005 §3.1](../issues/ISSUE-005-phase1-upstream-inventory.md)）：它们的消费者
	// 都在 P2/P3 调度（高峰倍率影响成本排序、独占与平台影响候选过滤），P1 无消费者。
	// 需要时按 payload 回填结构化列即可，不丢数据。
	PeakEnabled     bool
	PeakMultiplier  float64
	PeakStart, PeakEnd string
	SubscriptionType string
	Platform        string // openai / anthropic 等
	IsExclusive     bool
	Meta            SourceMeta
}

// CatalogModel（FR-126，P1）：渠道模型目录的一行 → channel_model_catalog
// **无 token 上界字段**——那是"可路由模型"（models 表）的必填项，目录不需要，
// 这正是目录必须独立于 models 的原因（[02 §1.3](./02-data-model.md)）。
type CatalogModel struct {
	ModelName    string  // 上游原始名 → channel_model_catalog.model_name
	InputPrice   float64 // → channel_model_catalog.input_price（归一美元）
	OutputPrice  float64 // → channel_model_catalog.output_price
	Meta         SourceMeta
}

type SubscriptionQuota struct {
	OwnerKey       string    // 共享额度归集键：Sub2API 为 (user,group)；无 group 概念的站型按 (user,plan)（FR-035）
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
	// ⏭ 下面三个字段（含 FixedFeeUSD/DurationDays）都是 P4 订阅制的形状，
	//    取自一家**已移出支持范围**的自研站的实测（原 §3.3）。留着是因为 FR-033/036/057
	//    还引用它们，但**做 P4 时必须按当时真实纳管的站型重新实测** —— 现役两族都没有
	//    这些概念，照这个形状写会得到一组没有数据源的列（[02 §0.3](./02-data-model.md) 同理）。
	PrimarySource   string   // 额度来源优先级（订阅先扣还是余额先扣，FR-033/8.5）
	SecondarySource string
	ManualResetPolicy *ManualReset // 可主动重置额度（FR-036 的外生变量）
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
| 3 | 全未命中 | —— | **未知家族** → §7 接入流程，新建专属适配器 |

> 顺序即注册表切片的顺序：**判据越通用的放越前面**。新加的自研站放最后 —— 它的判据只认自己那一站，放前面只是让另外两族每次多一跳无谓的探测。

**二开兼容要点**：NewAPI 系即使命中顺序 1，用户 ID 头名仍可能不同，探测阶段不确定，`Detect` 只归族；`UserIDHeader` 由 `Authenticate` 阶段 fan-out 试探（§3.1）。`quota_per_unit` 也逐站从 `/api/status` 读取，**不写死**（upstream-d.invalid 为 500000）。

上表的顺序、端点与命中特征都来自 §7bis 的站型注册表 —— `Detect` 遍历注册表而非另有一张平行的探测表（那张表曾是"加站型要改九处"里的第 2 处）。

---

## 3. 现役家族实现要点与字段映射

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
| `FetchGroups` | 由 `/api/pricing` 派生 | 顶层 `group_ratio` + 各模型 `enable_groups` **反转** | `RateMultiplier`、`AvailableModels`（FR-124） | FR-010/123/124 |
| `FetchPricing` | `/api/pricing`（**公开**） | `quota_type`、`model_ratio`、`model_price`、`completion_ratio`、`cache_ratio`、顶层 `group_ratio` | 价格版本（不可覆盖，FR-012） | FR-010/012/013/017 |
| `FetchModelCatalog` | `/api/pricing` | `model_name` + 上列价格字段 | `channel_model_catalog`（含 `billing_unit`） | FR-126 |
| `FetchSubscriptionQuotas` | —— | —— | `ErrUnsupported` | FR-034 |

**`/api/pricing` 的真实响应形状**（第 46 轮实测 20 个可达站点，**20/20 为下述形状，0 个为旧形状**）

```jsonc
{
  "data": [                        // ← **模型对象数组**，不是 {model_ratio:{...}} 字典
    { "model_name": "gpt-4o",
      "quota_type": 0,             // 0=倍率计价，1=按次固定价
      "model_ratio": 2.5,          // quota_type=0 时取此（相对基准价的倍率）
      "model_price": 0,            // quota_type=1 时取此（每次调用绝对美元价）
      "completion_ratio": 4,       // 输出价 = model_ratio × completion_ratio
      "cache_ratio": 0.5, "has_cache": true,
      "enable_groups": ["default","vip"]   // ← 分组可用模型的**唯一**来源
    }
  ],
  "group_ratio": { "default": 1, "vip": 0.8 },   // ← **顶层**，不在 data 内
  "version": "..."
}
```

> ⚠️ **历史陷阱（已修）**：早期文档记的是 `data.model_ratio` 为"模型名→倍率"字典。按字典解析时，43/44 个真实站点报"无 model_ratio"，`group_models` 更是**静默采到 0 行**却报 `ok`——因为 `model_ratio` 的键恒为空。适配器现同时兼容两种形状；命中旧字典形状时 `GroupModels` **留空而非虚构**"每个分组都有全部模型"。修复后价格覆盖率 2.3% → 86.4%，目录 2.3% → 100%，`group_models` 0 → 5546 行。

> ⚠️ **`quota_type` 决定口径，不可省略**：`quota_type=1` 的 `model_price` 是**每次调用的绝对美元价**（`billing_unit=per_call`），与倍率**数值区间重叠**（实测按次 0.08~0.56，倍率 0.685~30），无法从数值反推。实测某站 1369 个模型中 208 个（15%）为按次计价。落库必须带 `billing_unit`（[02 §1.3bis](./02-data-model.md)）。

> ⚠️ **`/api/user/token` 是"重新生成"而非"读取"** —— 实测每次调用都返回新令牌并**立即作废旧令牌**。本版本 `/api/user/self` 不回显 `access_token`，无法惰性"读不到再建"。采集器**只在初始化时调用一次并持久化**，运行时**禁止**重新生成（§5 状态机 N-1）。

**Capabilities：**
```
{account: supported, keys: supported, groups: supported, pricing: supported,
 model_catalog: supported, subscription_quotas: unsupported}
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
{account: supported, keys: supported, groups: supported, pricing: degraded,
 model_catalog: supported, subscription_quotas: **unsupported（一期）**}
```
（价格倍率经 `/api/v1/groups/available.rate_multiplier` 与分组耦合，非独立价格表，标 `degraded`。）

### 3.3 全自研站（**已无现役样本**，第 48 轮）

此前这里有一份闭源自建平台的逐字段实测表（专属适配器、micros 额度单位、7 天 JWT 无 refresh）。
**该站已移出支持范围**（2026-08-29 决定，见 [00 §3bis](./00-overview-and-milestones.md)），
适配器与注册一并删除，故本节不再有具体站点。

**但接入路径保留且已抽象成清单** —— 见 §7bis「全自研站怎么接」。那份清单里的每一条
都来自这次实测：命名空间与开源项目零重叠、额度单位自成一套、指纹在 JWT claim 里而不在
JSON 顶层字段里、无令牌端点只能账密重登。这些不是那一站的特例，而是"自建面板"这个类别
的常见形态，所以清单值得留，样本不必留。

⚠️ **接新的自研站时不要照本节的历史结论写代码** —— 它描述的是另一家站。
按 §7 的流程重新实测，按 §7bis 的清单落字段。历史探针记录仍在
[ISSUE-002](../issues/ISSUE-002-collector-adapter-design.md) 与
[ISSUE-003](../issues/ISSUE-003-runtime-feedback-requirement-candidates.md)（保留原站名，
那是当时用了什么方法的史实）。

### 3.4 能力矩阵总表

> 这张表**逐格**被 `adapters_test.go` 的 `TestCapabilitiesMatchDocMatrix` 对照（AC-28 的核心判据）。
> 加一族要同时改这里与那张表 —— 那条断言对"没进表的家族"会报错而不是空过。

| Capability | NewAPI | Sub2API |
| --- | --- | --- |
| `account` | supported | supported |
| `keys` | supported | supported |
| `groups` | supported | supported |
| `subscription_quotas` | **unsupported** | **unsupported（一期）** |  ⏭ 订阅制整体移入二期（[15 §1.2](./15-scope-and-preflight.md)）；`Capabilities()` 的声明必须与 `FetchSubscriptionQuotas` 返回 `ErrUnsupported` 一致，否则 [AC-28](./14-acceptance-matrix.md) 判不通过 |
| `pricing` | supported（公开） | degraded |
| `model_catalog`（**P1 新增**） | supported（`/api/pricing` 已含全量模型与价格） | supported（`/api/v1/groups/available` 带分组模型） |
| 令牌与续期 | 系统访问令牌，长期，初始化一次生成 | JWT 24h + refresh 无密码续期 |
| 额度单位 | `quota/quota_per_unit` | USD 浮点 |
| 共享额度归集键 | Key 独立 | `(user,group)` |
| 超额计费 | N/A | **无超额**（用尽即阻断） |

### 3.4bis `Degraded` 的运行时行为（**P1 必需**，第 45 轮补）

> ⚠️ **此前 `Degraded` 只有一行定义**（"部分可采 / 需人工补全 / 数据陈旧"），**没说方法该返回什么**。而 [AC-28](./14-acceptance-matrix.md)/[AC-38](./14-acceptance-matrix.md) 都要求"`Capabilities()` 声明必须与实际返回一致"——`degraded` 既不是 `supported` 也不是 `ErrUnsupported`，照原文**无法判定通过与否**（开发视角审查第 45 轮）。

**约定：`Degraded` 是能力声明，不是运行时状态。** 声明 `degraded` 的能力，其 fetch 方法**照常返回数据与 `nil` 错误**，但：

| 情形 | 返回 | `sync` 的 item status |
| --- | --- | --- |
| 采到了部分字段 | 数据 + `nil`；`SourceMeta` 记 `Degraded: true` 与 `MissingFields []string` | `ok`，并在 `note` 列出缺哪些字段 |
| 一个字段都没采到 | 空切片/零值 + `nil`（**不是** `ErrUnsupported`） | `ok`，`rows=0`，`note` 说明"该站型无独立端点" |
| 请求本身失败（网络/401/5xx） | `nil` + 真实 error | `failed` |
| 该站型**根本没有这个对象** | `ErrUnsupported` | `unsupported` |

- **判定口径（供 AC-28/38 执行）**：`Capabilities()` 声明 `degraded` ⟺ 该方法**不得**返回 `ErrUnsupported`。声明 `unsupported` ⟺ 必须返回 `ErrUnsupported`。声明 `supported` ⟺ 不得返回 `ErrUnsupported` 且必备字段齐全。
- **三处 `degraded` 的具体含义**：
  - NewAPI `pricing`：无（它是 `supported`，`/api/pricing` 公开且完整）。
  - Sub2API `pricing`：倍率与分组耦合在 `/api/v1/groups/available`，**无独立模型价格表** → 目录里 `input_price`/`output_price` 可能为空，`MissingFields=["input_price","output_price"]`。
  - 自研站的 `pricing` / `model_catalog` **多半也是 `degraded`**：自建面板常把模型与价格并入套餐页而没有独立目录端点 → 只能取到套餐维度的模型名，逐模型单价缺失。这一格该填什么由实测定，**不要照抄** —— 但要记得 `degraded` ≠ `unsupported`（判定口径见上一条）。
- **`MissingFields` 的用途**：`inventory` 的异常项计数把它计入"待人工补录"（FR-011：采不到即人工录入 + 标来源 + 7 天有效期），而不是当成故障。

---

## 4. 写入 [02 数据模型](./02-data-model.md) 的目标表

[02 数据模型](./02-data-model.md) 已定义下列表，本节给出采集器 → 02 的**写入映射**（表名对齐 02 实际结构）。采集器**只写这些表**，请求路径不读原始表而读内存快照（[01 §5](./01-architecture.md#5-数据面控制面切分)）。

| 02 实际表（[02](./02-data-model.md)） | 写入方法 | 关键列 | FR |
| --- | --- | --- | --- |
| `channels`/`upstream_accounts`/`upstream_keys`/`models` | Detect + Fetch* | 资源身份登记，不以渠道名代身份；`site_family`、`external_user_id` | FR-002 |
| `price_versions` | `FetchPricing` | 币种、计费单位、来源、查询/生效时间；**不可覆盖版本** | FR-012/013 |
| `price_change_log` | `FetchPricing` **同一事务** | 见下方「价格变更留痕」 | FR-014/017 |
| `balance_signals` | `FetchAccount` | `last_confirmed_balance`、`confirmed_at`、`balance_state`、`quota_status`、`signal_kind`、`signal_evidence`。⚠️ **不写 `known_consumption_since` 与 `conservative_floor`**，见下方列归属 | FR-020/024/026 |
| `upstream_keys` + `collector_snapshots` | `FetchKeys` | **P1 起写结构化列**：`remain_quota_usd`、`used_quota_usd`、`rpm_limit`、`concurrency_limit`、`quota_synced_at`、`expired_time`、`model_limits`、`channel_group_id`（[02 §1.3](./02-data-model.md)）；**用量历史**另写 `collector_snapshots(scope_type='key')` 的 payload。<br>⚠️ **第 44 轮修正**：上一版写"key 级 `remain_quota/...`"——`upstream_keys` **此前根本没有这些列**（既有错误，非笔误），采回的额度只能塞 payload，"这把 Key 还剩多少"查不出来。P1 补列后本行才成立 | FR-021/028/031、**FR-122/125/127** |
| `channel_groups` + `group_models` | `FetchGroups` | `group_ref`、`rate_multiplier`（结构化列）；`AvailableModels` → `group_models.model_name`（FR-124）。**高峰倍率/独占/平台仍只进 payload**（P1 无消费者，见 §1 结构注释） | **FR-123/124** |
| `channel_model_catalog` | `FetchModelCatalog` | `model_name`、`input_price`、`output_price`、`first_seen_at`（首次采到时写入，之后不变）、`last_seen_at`（每轮刷新）。**无 token 上界**——那是 `models` 的必填项 | **FR-126** |
| `subscription_plans`（含 `rate_multiplier`/`peak_*`） | `FetchGroups` + `FetchSubscriptionQuotas`（套餐维度） | 分组/高峰倍率、固定费用、有效期、支持模型、续订状态、**`usable_multiplier`/`actual_multiplier`** 双倍率 | FR-010/033、参数14 | ⏭ **二期** |
| `user_subscriptions` + `subscription_quota_windows` | `FetchSubscriptionQuotas`（实例维度） | `(ext_user_id, group_id)` 共享归集、周期额度/已用/剩余/重置、`primary/secondary_source`、`active_reset_*`、`overage_rule(no_overage_block for sub2api)` | FR-034/035/036、8.5 | ⏭ **二期** |
| `collector_credentials` | `Authenticate` 副产物 | 令牌/refresh/账密（一期明文，FR-113），脱敏引用入日志（FR-094） | FR-113 |
| `collector_snapshots`（内嵌 `data_source/fetched_at/valid_until`） | 所有 Fetch* | source、endpoint、fetched_at、valid_until（人工 +7d）；**陈旧性不落列**，查 `collector_snapshots_v.is_stale` 视图（[02 §7](./02-data-model.md)） | FR-011 |

**`balance_signals` 的列级写入归属**（第 41 轮：采集器与余额下限 worker **都写 `conservative_floor`**，
而 worker 会额外扣掉在途预留与安全储备 —— 采集器后写一次就把保守值抹回标称值。
余额只剩 $2、在途 $5 时，selector 会看到一个正数下限并继续放行付费请求。
与 [05 §5bis.0](./05-scheduling-and-operations.md) 给 `resource_health` 定的分工同一套办法）：

| 列 | 唯一写入者 | 说明 |
| --- | --- | --- |
| `last_confirmed_balance`、`confirmed_at` | **采集器** `FetchAccount` | 上游报的余额与采到的时刻，事实值 |
| `balance_state`、`quota_status` | **采集器** | 五态/配额态判定 |
| `signal_kind`、`signal_evidence` | **采集器** | 识别到"余额不足"时的信号类型与证据（[05 §5.3](./05-scheduling-and-operations.md)） |
| `known_consumption_since`、`conservative_floor` | **余额下限 worker**（每 5min，[05 §5bis.2](./05-scheduling-and-operations.md)） | **派生值**，由确认点 + 已终结消耗 + 在途预留 + 安全储备算出 |

- **采集器的 `UPDATE` 语句必须逐列列出，不得整行覆盖**：写成 `INSERT ... ON CONFLICT DO UPDATE SET
  (全部列) = ...` 就会把 worker 刚算好的下限一起冲掉。
- **顺序无关**：两方各写各的列，谁先谁后都不影响结果——这正是按列切分而非加锁的目的。

**价格变更留痕**（第 40 轮补：`price_change_log` 的 `from_version_id`/`to_version_id`/`direction`
建好后**零引用**——没有任何写入方，FR-014「降价需再次确认」与 FR-017「价格变化记录和影响范围」
双双落空；`GET /admin/prices/changes` 也就无数据可查）：

`FetchPricing` 每次插入新 `price_versions` 行时，**同一事务**内比对上一版并留痕：

| 情形 | `direction` | `confirmed` | 后续动作 |
| --- | --- | --- | --- |
| 新价 > 旧价 | `increase` | `true` | 直接生效（涨价照单全收，不需确认） |
| 新价 < 旧价 | `decrease` | **`false`** | ⚠️ **不直接采信**（FR-014/AC-03）：降价可能是采集页面改版误读。**`price_versions.confirmed` 也置 `false`**（两处必须一致），由一次真实扣费与预估一致后置 `true`（[05 §4.4](./05-scheduling-and-operations.md) 计费对账），或人工 `POST /admin/prices/changes/{id}/confirm`（该端点同事务改两张表） |
| 价格未变但 `queried_at` 刷新 | `confirmed` | `true` | 仅刷新新鲜度，不改判 |
| 采集失败/超期未采到 | `stale` | `false` | 不插 `price_versions`，只记一条 `stale` 留痕 + P3 告警（`category='data_stale'`） |

- **比对口径**：`(input_price, output_price, cache_price, billing_unit)` 四项任一不同即算变更；
  `billing_unit` 变了要**先归一再比大小**，否则 `per_1k_token` 换成 `per_1m_token` 会被误判成暴涨 1000 倍。
- **首次采到该 `(channel, model)`**：`from_version_id` 为 NULL、`direction='confirmed'`，不算变更。
- **`decrease` 未确认期间照常使用新价**：保守方向是"按更贵的算"，而低价会让 selector 更倾向选它——
  故未确认的降价**不参与"低价优选"排序**（与 §1.1 序 6 价格陈旧的处置一致），只作保底。

> **无 group 概念的站型怎么落归集键**：02 的 `user_subscriptions` 用 `(ext_user_id, group_id)` 表达共享额度。而自建面板常没有 group 这层，只有套餐 —— 那种站的 `group_id` 列填**套餐标识**（按 `(user, plan)` 聚合）；采集器负责把家族差异映射到统一列。⏭ 属 P4，做的时候按当时真实纳管的站型重新确认。

> **双倍率落库分工**：`subscription_plan.用满倍率 = 固定费用÷周期额度×分组倍率`（调度排序，FR-057），`subscription_instance.实际倍率 = 固定费用÷实际消耗×倍率`（账务报表，FR-058）。采集器只提供两者的**原料**（`FixedFeeUSD`、`DurationDays`、`LimitUSD`、`UsedUSD`、`RateMultiplier`），倍率计算由 `steward`/账本完成，不在采集器内。

---

## 5. 凭证生命周期状态机（核心风险区）

**任何家族都有"凭证互斥作废"风险**，这是采集器最容易出事的地方。凭证操作按站型分套状态机（现役两套 + 账密重登一套，§5.3），均以 `collector_credential` 持久化为唯一真相。

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

### 5.3 账密重登（**无令牌端点的站型**，当前无现役样本）

自建面板常只有登录表单、没有任何令牌端点 —— 那种站唯一的续期方式就是拿账密重新登录。
这条路径的代码通路留着（`Registration.PasswdCredType` → `Credential.Username/Password` →
`CredTypeFor` 的 `hasPasswd` 分支 → 库侧 `cred_type='account_password'`），
现役两族都不走它。形状如下：

```
[持有令牌] --正常采集 (Authorization: Bearer)--> [持有令牌]
[持有令牌] --剩余有效期 < RefreshLead--> [重登中] --POST <该站的登录端点> {username,password}--> [持有新令牌]
[持有令牌] --401--> [重登中]

约束: 必须持有账号密码才能续期（无 refresh 路径的固有约束）；账密一期明文存储(FR-113)。
阈值: 由 Registration.RefreshLead 定，按该站令牌有效期取 —— 有效期越长阈值可越宽松
      （曾实测过一家 7 天 JWT 的站，取 <1 天重登，约每周 1 次，风控压力很小）。
```

⚠️ **`RefreshLead` 与 `Refresher` 必须同时有** —— `registry_test.go` 第 4 条断言双向钉住：
只声明阈值不实现 `Refresher` = 判定该续期却没有刷新器，到期一路 401。

---

## 6. 通用防护

| 项 | 要求 | 来源 |
| --- | --- | --- |
| **限速** | 同站点请求强制最小间隔（参考 all-api-hub `minIntervalLimiter`），避免触发风控；查询频率默认见 PRD §11（价格 6h / 余额校对 5～15min / 订阅 1h），可配置 | FR-116、参数10 |
| **无盾确认** | 接入前必须确认 `turnstile_check:false`（NewAPI）/ `turnstile_enabled:false`（Sub2API）。**开盾站点服务端采集不可行**，须转人工录入。已实测 upstream-d.invalid、molifang 均无盾 | ISSUE-002 §5 |
| **凭证互斥作废** | 见 §5 的状态机：NewAPI 令牌只生成一次、Sub2API 按账号串行刷新、无令牌端点的站型账密重登 | ISSUE-002 §4 |
| **降级一致性** | 任何字段采集失败一律落 FR-011：人工录入 + 数据来源标注 + **7 天有效期** + 过期按"订阅数据未知"降级（订阅状态机进"数据未知"，PRD §9.3）。**不允许用陈旧数据驱动订阅倾斜** | FR-011、AC-28 |
| **余额信号输入** | 采集器提供"账户/Key 余量是否归零"的判据，供 `steward` 的余额不足信号自适应识别框架（错误文案正则 + 失败信号 + 余量归零）判定资源置"耗尽" | FR-027、AC-29 |
| **额度归一** | 各家族的额度单位（NewAPI `quota/quota_per_unit`、Sub2API USD 浮点，自建站还可能是 micros 之类自成一套的单位）**一律在适配器内归一为数值美元**，一期各币种 1:1（币种仅名称）。归一必须在适配器里做完 —— 让单位漏到上层，等于让每个消费方各猜一次换算基数 | FR-018、AC-17 |

---

## 7. 未知家族接入流程

1. `Detect` 依次比对 `/api/status`、`/api/v1/settings/public`、`/api/public/site-config`，全未命中 → `FamilyUnknown`。
2. 抓取前端实际调用的接口（`performance.getEntriesByType('resource')` 思路取样），人工确认字段语义后写专属适配器。
3. 确无接口 → 按 FR-011 人工录入，标注来源、7 天有效期，过期按"订阅数据未知"降级。

### 7bis 站型注册表（**加站型的唯一落点**，第 47 轮）

> 上游站点异构程度高且**会继续增加**：除现役两族本身，实测已有魔改站（`veloera`/`rix-api` 是 NewAPI 的二开），也会遇到全自研平台（已实测过一家，虽已移出支持范围，但那类站还会有）。这一节定义"加一个站型要动哪里"。

**此前散在九处**，每处漏改的后果不同：

| # | 位置 | 漏掉的后果 |
| --- | --- | --- |
| 1 | `Family` 常量 | 编译错 —— 会被发现 |
| 2 | `detectSteps` 探测表 | `Detect` 返回 unknown，站点接不进来 |
| 3 | `adapterFor` 装配 | 采集报"无对应适配器" |
| 4 | `authHeaders` 鉴权头 | **静默 401** —— 编译过、单测过，跑起来才发现一个头都没带 |
| 5 | `NeedsRefresh` 续期阈值 | **静默过期** —— 不报错、不 401，某次采集突然全挂才被发现 |
| 6 | 凭证必需字段校验 | 落到 default 分支，凭证登记被拒 |
| 7 | 凭证登记的 `credType` 映射 | `cred_type` 空串进库 |
| 8 | 导入侧的 `credType` 映射 | 同上，批量导入路径 |
| 9 | 导入侧的别名表 | 导出声明认不出来，"声明与探测不符"的比对静默失效 |

**收敛后：一份 `Registration` + 一个 `Adapter` 实现。** 判据是**只有逐家族真的不同的东西才进注册表** —— 各族一致的行为不是变化点而是常量，给它开字段只会让下一个站型以为自己必须填。据此第 4 处**被删掉而不是收进表**：收表当时的三个家族（含后来移出的那一族）鉴权头都是 `Bearer <token>` + 有头名则带用户 ID 头，那个 switch 表达的是零个变化点。

| `Registration` 字段 | 取代原来的 | 备注 |
| --- | --- | --- |
| `Family` / `DisplayName` | 1 | `DisplayName` 同时是界面站型下拉的显示名（[09 §5.0](./09-admin-api.md) 的 `/admin/site-families`） |
| `ProbePath` / `Match` / `Extract` | 2 | 切片顺序**就是**探测顺序（§2 的表）；不用各文件 `init()` 自注册——那会让顺序被一次重命名悄悄改掉 |
| `Aliases` | 9 | ⚠️ **只准填实测见过的自称**（[CLAUDE.md](../../CLAUDE.md) §1）：凭想象加别名会让本该报出来的声明错变成静默采信 |
| `CredType` / `RequiresUID` / `PasswdCredType` / `CredNote` | 6+7+8 | 校验与选型合成一次 `CredTypeFor()` 调用，两处消费点共用 |
| `RefreshLead` | 5 | `0` = 永不主动续期（NewAPI 的不变式 N-1，§5.1） |
| `New` | 3 | **是否支持续期不在注册表里声明** —— 由适配器有没有实现 `Refresher` 决定（类型断言），声明与实现因此不可能不一致 |

#### 魔改站怎么接

`veloera`/`rix-api` 改的是用户 ID 头名与分页信封，前者由 `Authenticate` 的 fan-out 试探（§3.1），后者 `unwrapDataList` 两个分支都吃 —— 所以魔改站 = **同一个 `New`，只加 `Aliases`**，不写新适配器。

**先判断是魔改还是自研**：能命中 `/api/status` 或 `/api/v1/settings/public` 的就是魔改（顺着上面走），全未命中才是自研（走下一段）。判错的代价不对称 —— 把魔改站当自研站接会白写一个适配器，而把自研站当魔改站接会让字段映射整片错位，且**不报错**。

#### 全自研站怎么接

**落点只有两个文件**：`register_<族>.go`（一份 `Registration`）+ `<族>.go`（一个实现 `Adapter` 的类型）。
`registry.go`、`detect.go`、`auth.go`、`httpx.go`、界面、库结构**都不用改** —— 下表逐格说明为什么不用改，
以及每个决策点该落在哪个字段。

> **刻意不留 stub 适配器。** 一个没有真站点的 `selfhosted.go` 骨架正是
> [CLAUDE.md §1](../../CLAUDE.md) 那个坑：它编码的是"自研站长什么样"的猜测，
> 而它永远不会推翻那个猜测。所以留清单不留骨架。

| 决策点 | 落在哪 | 填错时哪道守卫会红 |
| --- | --- | --- |
| 判族用哪个公开端点 | `ProbePath` | 空 → `TestRegistrationsComplete`；探测不到不会红，**只会静默归 unknown**（所以要先手工 curl 确认） |
| 指纹怎么认 | `Match(m, raw)` | 空 → 同上。指纹**不在 JSON 顶层**时读第二个参数 `raw`：解析失败时 `m` 为 nil 而 `raw` 仍有原文（`TestDetectPassesRawBodyToMatch` 钉住这条通路） |
| 探测顺序 | 切片位置，**放最后** | 不红。判据只认自己一站，放前面只是让另外两族多一跳 |
| 版本 / 无盾 / 额度换算基数 | `Extract` | 不填不红（都是可选信息），但无盾没确认就接入违反 §6 |
| 导出数据里的自称 | `Aliases` | 重复或非小写 → `TestAliasesUniqueAcrossFamilies`。⚠️ **只填实测见过的值** |
| 凭证形态 | `CredType` + `RequiresUID`/`PasswdCredType` + `CredNote` | 任一为空（除两个可选项）→ `TestRegistrationsComplete`。库侧 `cred_type` 的 CHECK 要同步加一条迁移，否则**登记时才报约束冲突** |
| 只能账密重登 | `PasswdCredType` 非空 | 不红。填了它，界面的账号/密码字段**自动出现**（`SiteFamilyInfo.allows_password` → `CredsView` 的 `allowsPassword`），不用改前端 |
| 要不要主动续期 | `RefreshLead` + 适配器实现 `Refresher` | 单边 → `TestRefreshLeadMatchesRefresherImplementation`（双向）。`0` = 永不主动续期 |
| 额度单位（micros 之类） | 适配器内部，**归一为美元后再返回** | 不红。漏了会让上层每个消费方各猜一次换算基数（§6 末行） |
| 采不到的对象 | `Capabilities()` 里标 `unsupported`，方法返回 `ErrUnsupported` | `TestUnsupportedDeclarationsReturnErrUnsupported`（双向）+ `TestCapabilitiesMatchDocMatrix`（要同时写进 §3.4 那张表，漏了会报"不在本表里"） |
| 采得到但不全 | 标 `degraded`，返回数据 + `nil` + `SourceMeta.MissingFields` | 同上。⚠️ `degraded` **不得**返回 `ErrUnsupported`（§3.4bis 的判定口径） |
| `Family` 常量 | `collector.go` 的常量块 | 加了常量忘了注册 → `TestEveryFamilyConstantIsRegistered`（它扫源码，所以不用维护清单） |

**接入前必须先做的两件事**（顺序不能反）：

1. **手工打一遍真站点**，把 `ProbePath` 的响应原文、余额端点的字段名与**类型**（数字还是字符串）记下来。
   照文档或照想象写夹具是本仓库踩过两次的坑（[CLAUDE.md §1](../../CLAUDE.md)）。
2. **确认无盾**（§6）。开盾站服务端采集不可行，要走 FR-011 人工录入，不是写适配器。

> **上表的"哪道守卫会红"已逐格实测**（2026-08-30）：真往注册表注入一族 `probe`
> 假家族，按每格各犯一次错，看点名的守卫是否真的红。六格全按表所述地红，
> 且每条错误信息都点出后果（不只是"断言失败"）。**其中 `TestCapabilitiesMatchDocMatrix`
> 在每种注入下都红** —— 注册一族而不写进 §3.4 矩阵是不可能的，它是最硬的一道。
> `PasswdCredType` 那格另用真 Chrome 验过三轮（声明 → 撤回 → 再看），
> **前端产物指纹三轮不变**而字段随后端声明出现/消失。测量记录见
> [P1-evidence §5.14](../acceptance/P1-evidence.md)。
>
> 这段验证的必要性就在表本身：**"填错了会被 X 拦下"是个断言，没红过的断言不算断言。**
> 表写好而守卫其实空转，比没有表更糟 —— 接站的人会照着表省掉自查。

**"漏一处"的检出**（`registry_test.go`，全是对注册表的静态断言，不新增任何 mock）：

1. **每个 `Family` 常量都注册了** —— 扫源码取常量而非手写清单，因为"忘了加清单"与"忘了加注册"一样静默；`unknown` 反向断言**不得**有注册（有适配器意味着会去猜家族）
2. **每条注册的必填字段非空** —— `ProbePath`/`Match`/`CredType`/`CredNote`/`New`/`DisplayName`
3. **`Aliases` 无跨家族重复、且全小写** —— 重复不报错而是让导入侧的声明比对随机偏向注册顺序在前的那个
4. **`RefreshLead` 与 `Refresher` 实现双向一致** —— `>0` 无实现 = 到期即 401；`==0` 有实现 = 不变式 N-1 被破（每次采集前把自己的令牌作废）

---

## 8. 与其他篇章的边界

- **不含**请求链路能力：内容感知 TTFT、动态期限接管、mid-stream 取消传播、`errorMessage` 归并对账、同资源隐藏重试补算 —— 均属 `executor`/`ledger`（[01 §2](./01-architecture.md#2-sla-core-内部模块)、[00 硬约束 4/5/7](./00-overview-and-milestones.md#2-硬约束清单开发期不可违反)），本篇不涉及。
- **依赖** 02 定型上表结构；**被依赖** 于 `steward`（余额信号、订阅倾斜三道闸）与 `selector`（配额状态 unknown 默认排除 FR-118 —— 但采集器只负责采回状态，过滤在 selector 做）。
- 采集器是异步控制路径，任何抖动不阻塞同步决策；数据经后台快照供请求路径只读（[01 §5](./01-architecture.md#5-数据面控制面切分)）。
