// Package collector 是采集侧契约与三家族实现（04）。
//
// 定位：异步旁路控制路径，任何抖动**不阻塞**同步决策路径（01 §5）。
// 边界：不含请求链路能力（TTFT/接管/取消属 executor，04 §8）。
//
// 一条总原则（04 开头）：上游站点异构程度高，**必须按站型分流**。
// NewAPI/Sub2API 是通用开源项目，同族站点复用同一适配器但接入前必须先探测确认；
// **全自研站**（闭源面板、自建平台）一站一适配器、不可复用、不作探测基准 ——
// 接入清单见 04 §7bis「全自研站怎么接」。
// 任何不支持的字段返回 unsupported，**不静默留空**。
package collector

import (
	"context"
	"errors"
	"time"
)

// SupportLevel 对应 04 §1 的"支持/降级/不支持"。
type SupportLevel string

const (
	// Supported：有该对象且可自动采集。
	Supported SupportLevel = "supported"
	// Degraded：部分可采 / 需人工补全 / 数据陈旧。
	//
	// ⚠️ 运行时语义见 04 §3.4bis：Degraded 是**能力声明**而非运行时状态 ——
	// 声明它的方法照常返回数据与 nil error，用 SourceMeta.Degraded 与
	// MissingFields 说明缺什么。**不得**返回 ErrUnsupported，
	// 否则 AC-28/AC-38 的"声明与实现一致"判定会失败。
	Degraded SupportLevel = "degraded"
	// Unsupported：该站型无此对象（如 NewAPI 无订阅）。
	Unsupported SupportLevel = "unsupported"
)

// Family 是站型家族。
//
// 加一族的落点只有两处：这里一个常量 + 一份 Registration（04 §7bis）。
// registry_test.go 的第一条断言扫本文件的常量并逐个查注册表，所以
// "加了常量忘了注册"会红，反之 unknown 有注册也会红。
type Family string

const (
	FamilyNewAPI  Family = "newapi"
	FamilySub2API Family = "sub2api"
	FamilyUnknown Family = "unknown"
)

// Capability 是除 detect/authenticate/capabilities 外的每个 fetch 能力。
type Capability string

const (
	CapAccount Capability = "account"
	CapKeys    Capability = "keys"
	CapGroups  Capability = "groups"
	// CapSubscriptionQuotas ⏭ P4：一期**所有站型**一律 Unsupported
	// （订阅制整体推迟）。此前有站型声明 supported 而实现返回
	// ErrUnsupported —— 自相矛盾且 AC-28 必挂（04 §1 第 18 轮记录）。
	CapSubscriptionQuotas Capability = "subscription_quotas"
	CapPricing            Capability = "pricing"
	// CapModelCatalog 是 P1 新增（FR-126）。
	CapModelCatalog Capability = "model_catalog"
)

// CapabilityMap 是站型的静态能力声明。
type CapabilityMap map[Capability]SupportLevel

// ErrUnsupported 是"该站型无此对象"的哨兵错误。
//
// 调用方据此登记 Unsupported，**不留空**（04 §1）。
// 与 Degraded 的区别见 04 §3.4bis：degraded 不返回本错误。
var ErrUnsupported = errors.New("collector: 该站型不支持此能力")

// ErrPrecondition 标记"一次采集在发出第一个上游请求之前就失败了"。
//
// 站型未知、连接池取不到连接、凭证没登记 —— 这三类都是纯本地失败。
// 单独立哨兵是为了让调用方能区分"没打上游"和"打了上游但失败"：
// sync 的最小间隔限流意在保护上游，本地失败不该消耗这个窗口，
// 否则「建渠道 → 采集 → 提示缺凭证 → 登记 → 再采集」这条首跑路径
// 会被自己的失败挡 60 秒（P1-evidence §4 第 15 项）。
var ErrPrecondition = errors.New("collector: 采集前置条件不满足（未触达上游）")

// SourceMeta 是每份数据的来源元信息（FR-011/012/116）。
type SourceMeta struct {
	// Source: "api" | "manual" | "derived"
	Source string
	// Endpoint 是实际命中的端点，便于审计。
	Endpoint string
	// FetchedAt 查询时间。
	FetchedAt time.Time
	// ValidUntil 过期时间；人工录入默认 FetchedAt+7d（FR-011）。
	ValidUntil time.Time
	// Stale 是否已过期。**内存态判定，不落库** ——
	// 库侧查 collector_snapshots_v 视图（02 §7）。
	Stale bool

	// ── Degraded 能力的实际结果（04 §3.4bis）──
	// 声明 degraded 的能力照常返回数据与 nil error，用这两个字段说明缺了什么，
	// 供 sync 的 item.note 与 inventory 的"待人工补录"计数使用（FR-011）。
	Degraded bool
	// MissingFields 缺失的字段名，如 ["input_price","output_price"]。
	MissingFields []string
}

// ManualValidity 是人工录入数据的有效期（FR-011：7 天后视为过期）。
const ManualValidity = 7 * 24 * time.Hour

// NewAPIMeta 构造自动采集的来源元信息。
func NewAPIMeta(endpoint string, at time.Time) SourceMeta {
	return SourceMeta{Source: "api", Endpoint: endpoint, FetchedAt: at}
}

// DetectResult 是站型探测结果。
type DetectResult struct {
	Family  Family
	Version string // 如 NewAPI "v1.0.0-rc.19"、Sub2API "0.1.163"
	// NoShield 表示 turnstile 已关闭。
	// ⚠️ **开盾站点服务端采集不可行**，须转人工录入（04 §6 无盾确认）。
	NoShield bool
	// UserIDHeader 是 NewAPI 二开 fan-out 命中的头名，其余家族为空。
	// Detect 阶段**不确定**它，由 Authenticate 阶段试探（04 §2）。
	UserIDHeader string
	// QuotaPerUnit 是 NewAPI 系的额度换算基数，逐站从 /api/status 读，
	// **不写死**（upstream-d.invalid 为 500000，别家不一定）。
	QuotaPerUnit float64
	Meta         SourceMeta
}

// Credential 是登记的采集凭证（明文，FR-113）。
type Credential struct {
	ChannelID int64
	Family    Family
	// CredType 由 Registration.CredTypeFor 定，取值范围就是各注册的
	// CredType 与 PasswdCredType（现役：newapi_access_token / sub2api_jwt，
	// 另留 account_password 一格）。库侧 CHECK 与它同步，见 migrations/017。
	CredType     string
	AccessToken  string
	RefreshToken string
	// Username/Password 服务于**没有令牌端点的站**：唯一续期方式是账密重登。
	// 全自研站常是这个形态（自建面板往往只有登录表单），所以这条路留着 ——
	// 见 Registration.PasswdCredType 与 04 §7bis。现役两族都不用。
	Username string
	Password string
	// ExternalUserID 是 NewAPI 的数字用户 ID（New-API-User 头必需）。
	ExternalUserID string
	// UserIDHeaderName 是二开 fan-out 命中的头名（04 §3.1）。
	UserIDHeaderName string
	TokenExpiresAt   time.Time
	// RefreshLockKey identifies credentials for the same upstream account.
	RefreshLockKey string
	// BaseURL 是该渠道的站点地址（随渠道登记而来）。
	BaseURL string
	// QuotaPerUnit 是 NewAPI 系的额度换算基数，来自 Detect。
	// **不设默认值**：猜错会让余额差几十万倍（newapi.go FetchAccount）。
	QuotaPerUnit float64
}

// Session 是鉴权后的会话句柄。
type Session struct {
	ChannelID int64
	Family    Family
	BaseURL   string
	// Token 是当前有效的访问令牌。
	Token string
	// UserIDHeader/ExternalUserID 供 NewAPI 系每次请求带上（04 §3.1：
	// 只带 Cookie 会 401）。
	UserIDHeader   string
	ExternalUserID string
	// ExpiresAt 令牌到期时刻；到期前阈值内须续期（04 §5）。
	ExpiresAt time.Time
	// QuotaPerUnit 见 DetectResult。
	QuotaPerUnit float64
}

// Account 是账号级数据（FR-020/024/026）。
type Account struct {
	UserID     string
	BalanceUSD float64 // 已归一为美元（FR-018，一期 1:1）
	UsedUSD    float64
	Meta       SourceMeta
}

// RateLimit 是限流快照（FR-028）。
type RateLimit struct {
	RPM         int
	Concurrency int
	// Windows 是 Sub2API 的 5h/1d/7d 窗口用量（元数据，进 payload）。
	Windows map[string]WindowUsage
}

// WindowUsage 是一个滚动窗口的用量。
type WindowUsage struct {
	LimitUSD    float64
	UsageUSD    float64
	WindowStart time.Time
}

// Key 是 Key 级数据（FR-003/028/031/122/125/127）。
type Key struct {
	// KeyRef 是脱敏引用，**不含明文**（FR-094）。
	KeyRef         string
	RemainQuotaUSD *float64
	UsedQuotaUSD   *float64
	Unlimited      bool
	ExpiredAt      *time.Time
	ModelLimits    []string
	RateLimit      RateLimit
	// GroupRef 是所属分组（→ upstream_keys.channel_group_id，FR-123）。
	GroupRef string
	// RequestCount 是上游侧累计请求数（NewAPI 有），进 payload 时序。
	RequestCount int64
	Meta         SourceMeta
}

// Group 是分组数据（FR-123/124）。
type Group struct {
	GroupRef string // → channel_groups.group_ref
	// RateMultiplier 分组倍率 → channel_groups.rate_multiplier
	RateMultiplier float64
	// AvailableModels 该分组可获取的模型（FR-124）→ group_models.model_name
	AvailableModels []string
	RPMLimit        int

	// ── 以下字段 P1 采集但**不落结构化列**，只进 collector_snapshots.payload ──
	// 理由（ISSUE-005 §3.1）：消费者都在 P2/P3 调度（高峰倍率影响成本排序、
	// 独占与平台影响候选过滤），P1 无消费者。需要时按 payload 回填即可，不丢数据。
	PeakEnabled        bool
	PeakMultiplier     float64
	PeakStart, PeakEnd string
	SubscriptionType   string
	Platform           string
	IsExclusive        bool

	Meta SourceMeta
}

// CatalogModel 是渠道模型目录的一行（FR-126）→ channel_model_catalog。
//
// **无 token 上界字段** —— 那是"可路由模型"（models 表）的必填项，
// 目录不需要，这正是目录必须独立于 models 的原因（02 §1.3）。
type CatalogModel struct {
	ModelName   string
	InputPrice  float64
	OutputPrice float64
	// BillingUnit 是 InputPrice/OutputPrice 的口径，**不可省略**。
	//
	// 实测（第 46 轮，65 个真实站点）：NewAPI 同一个 /api/pricing 里
	// 混着两种口径，1369 个模型中 208 个（15%）是 per_call 绝对价，
	// 其余是 per_1m_token 倍率；而两者数值区间**重叠**
	// （按次 0.08~0.56 vs 倍率 0.685~30）。
	// 于是"看数值猜口径"不成立 —— 没有这个字段，目录里的价格就是个
	// 无单位的数字，把 $0.15/次 当倍率 0.15 排序会让最贵的模型显得最便宜。
	BillingUnit string
	Meta        SourceMeta
}

// Pricing 是价格采集结果（FR-010/012/013）。
type Pricing struct {
	// Models 逐模型价格；作用域 (channel, model)。
	Models []ModelPrice
	// GroupRatios 分组倍率（NewAPI 的 group_ratio）。
	GroupRatios map[string]float64
	Meta        SourceMeta
}

// ModelPrice 是一个模型的价格。
type ModelPrice struct {
	ModelName   string
	InputPrice  float64
	OutputPrice float64
	CachePrice  float64
	// BillingUnit 如 per_1m_token。⚠️ 成本计算必须按它缩放
	// （02 §3：漏缩放会把成本放大 100 万倍）。
	BillingUnit string
}

// SubscriptionQuota ⏭ P4。一期所有站型的 FetchSubscriptionQuotas
// 一律返回 ErrUnsupported，故本结构暂只保留占位定义。
type SubscriptionQuota struct {
	OwnerKey string
	PlanRef  string
	Meta     SourceMeta
}

// Adapter 是全部站型实现的统一契约（04 §1）。
type Adapter interface {
	// Detect 站型探测：公开、无鉴权，接入任何新站点的第一步（04 §2）。
	Detect(ctx context.Context, baseURL string) (DetectResult, error)

	// Authenticate 把凭证换成会话句柄（04 §5 三套状态机）。
	Authenticate(ctx context.Context, cred Credential) (Session, error)

	// FetchAccount 账号级：余额、已用、用户 ID（FR-020/024/026）。
	FetchAccount(ctx context.Context, s Session) (Account, error)

	// FetchKeys Key 级：额度、有效期、限流、模型权限（FR-003/028/031）。
	FetchKeys(ctx context.Context, s Session) ([]Key, error)

	// FetchGroups 分组：倍率、可用模型、限流（FR-123/124）。
	FetchGroups(ctx context.Context, s Session) ([]Group, error)

	// FetchSubscriptionQuotas ⏭ P4：一期所有站型返回 ErrUnsupported。
	FetchSubscriptionQuotas(ctx context.Context, s Session) ([]SubscriptionQuota, error)

	// FetchPricing 模型价格（FR-010/012/013）。
	FetchPricing(ctx context.Context, s Session) (Pricing, error)

	// FetchModelCatalog 渠道可用模型全量目录（FR-126，P1 新增）。
	//
	// 与 FetchPricing 的区别：Pricing 产出权威价格版本（不可覆盖，FR-012），
	// 本方法产出"上游有哪些模型"的清单 —— 两者可能来自同一端点，
	// 但落库目标不同（price_versions vs channel_model_catalog）。
	FetchModelCatalog(ctx context.Context, s Session) ([]CatalogModel, error)

	// Capabilities 静态能力声明：调度前即可知道该站能采什么。
	//
	// ⚠️ 必须与实际返回一致（AC-28/AC-38）：声明 supported 而实现返回
	// ErrUnsupported 即判不通过。
	Capabilities() CapabilityMap
}
