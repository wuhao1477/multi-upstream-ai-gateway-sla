package collector

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// NewAPIAdapter 是 NewAPI 系适配器（04 §3.1，upstream-d.invalid 已实测）。
//
// 同族站点复用本适配器，但**接入前必须先 Detect 确认**，且二开差异由
// 用户 ID 头名 fan-out 吸收（04 总原则）。
type NewAPIAdapter struct{ C *Client }

// NewNewAPIAdapter 构造适配器。
func NewNewAPIAdapter(c *Client) *NewAPIAdapter {
	if c == nil {
		c = NewClient(200 * time.Millisecond)
	}
	return &NewAPIAdapter{C: c}
}

func (a *NewAPIAdapter) Capabilities() CapabilityMap {
	return CapabilityMap{
		CapAccount:      Supported,
		CapKeys:         Supported,
		CapGroups:       Supported,
		CapPricing:      Supported, // /api/pricing 公开且完整
		CapModelCatalog: Supported, // 同一端点已含全量模型与价格
	}
}

func (a *NewAPIAdapter) Detect(ctx context.Context, baseURL string) (DetectResult, error) {
	return Detect(ctx, a.C, baseURL)
}

// Authenticate 确认凭证可用，并**试探出用户 ID 头名**。
//
// 04 §3.1 的关键约束：必须同时带 `New-API-User: <数字用户ID>`，只带
// Authorization 会 401；而二开站点改了头名，故首次鉴权要 fan-out 试探。
//
// ⚠️ 不变式 N-1：**运行时不调用 /api/user/token**（它是"重新生成"，
// 会作废正在使用的令牌）。令牌由运维一次性登记，本方法只验证与试探。
func (a *NewAPIAdapter) Authenticate(ctx context.Context, cred Credential) (Session, error) {
	if cred.AccessToken == "" {
		return Session{}, fmt.Errorf(
			"NewAPI 需要预先登记的系统访问令牌（不变式 N-1：运行时不可生成）")
	}
	if cred.ExternalUserID == "" {
		return Session{}, fmt.Errorf(
			"NewAPI 需要 external_user_id（用户 ID 头的值），否则必然 401")
	}

	// 已知头名则直接用，避免每次鉴权都 fan-out（每次试探都是真实请求，
	// 打七次会无谓消耗限速预算并增加风控暴露）。
	candidates := NewAPIUserIDHeaderCandidates()
	if cred.UserIDHeaderName != "" {
		candidates = append([]string{cred.UserIDHeaderName}, candidates...)
	}

	var lastErr error
	for _, hdr := range candidates {
		s := Session{
			ChannelID: cred.ChannelID, Family: FamilyNewAPI,
			BaseURL: cred.BaseURL, Token: cred.AccessToken,
			UserIDHeader: hdr, ExternalUserID: cred.ExternalUserID,
		}
		m, _, err := a.C.getJSONAuth(ctx, s, "/api/user/self")
		if err != nil {
			lastErr = err
			continue
		}
		// 命中判据：能取到 data 且里面有 quota 类字段
		d := unwrapData(m)
		if _, ok := d["quota"]; !ok {
			if _, ok2 := d["id"]; !ok2 {
				lastErr = fmt.Errorf("头名 %s 通过但响应无预期字段", hdr)
				continue
			}
		}
		s.QuotaPerUnit = cred.QuotaPerUnit
		return s, nil
	}
	return Session{}, fmt.Errorf(
		"七个用户 ID 头名全部试探失败（04 §3.1 fan-out）: %w", lastErr)
}

// FetchAccount 取账号余额与已用（FR-020/024）。
//
// 额度换算：金额(USD) = quota / quota_per_unit（04 §3.1）。
// quota_per_unit **逐站从 /api/status 读取，不写死**（upstream-d.invalid 是 500000）。
func (a *NewAPIAdapter) FetchAccount(ctx context.Context, s Session) (Account, error) {
	m, _, err := a.C.getJSONAuth(ctx, s, "/api/user/self")
	if err != nil {
		return Account{}, err
	}
	d := unwrapData(m)

	qpu := s.QuotaPerUnit
	if qpu <= 0 {
		// 没有换算基数就**不能猜**：猜 500000 而实际是 1 会让余额差 50 万倍，
		// 直接导致"有钱判没钱"或反之。宁可失败让运维补 Detect。
		return Account{}, fmt.Errorf(
			"缺少 quota_per_unit（应由 Detect 从 /api/status 读取），无法归一额度")
	}

	quota, _ := asFloat(d["quota"])
	used, _ := asFloat(d["used_quota"])
	acc := Account{
		UserID:     asString(d["id"]),
		BalanceUSD: quota / qpu,
		UsedUSD:    used / qpu,
		Meta:       NewAPIMeta("/api/user/self", time.Now()),
	}
	if acc.UserID == "" {
		acc.UserID = s.ExternalUserID
	}
	return acc, nil
}

// FetchKeys 取 Key 级额度、有效期、限流、模型权限（FR-003/028/031/122/125/127）。
func (a *NewAPIAdapter) FetchKeys(ctx context.Context, s Session) ([]Key, error) {
	m, _, err := a.C.getJSONAuth(ctx, s, "/api/token")
	if err != nil {
		return nil, err
	}
	qpu := s.QuotaPerUnit
	if qpu <= 0 {
		return nil, fmt.Errorf("缺少 quota_per_unit，无法归一 Key 额度")
	}

	items := asSlice(unwrapDataList(m))
	out := make([]Key, 0, len(items))
	now := time.Now()
	for _, it := range items {
		t := asMap(it)
		if t == nil {
			continue
		}
		k := Key{
			// KeyRef 是脱敏引用：用上游 id + name，**不含明文 key**（FR-094）
			KeyRef:    asString(t["id"]),
			Unlimited: asBool(t["unlimited_quota"]),
			GroupRef:  asString(t["group"]),
			Meta:      NewAPIMeta("/api/token", now),
		}
		if remain, ok := asFloat(t["remain_quota"]); ok {
			remain /= qpu
			k.RemainQuotaUSD = &remain
		}
		if used, ok := asFloat(t["used_quota"]); ok {
			used /= qpu
			k.UsedQuotaUSD = &used
		}
		// expired_time = -1 表示永不过期（NewAPI 约定），不是 1969 年
		if exp, ok := asFloat(t["expired_time"]); ok && exp > 0 {
			tm := time.Unix(int64(exp), 0)
			k.ExpiredAt = &tm
		}
		// model_limits 仅在 model_limits_enabled 为真时有意义 ——
		// 否则那串值是历史残留，当成权限限制会错误缩小候选集
		if asBool(t["model_limits_enabled"]) {
			k.ModelLimits = splitCSV(asString(t["model_limits"]))
		}
		if rc, ok := asFloat(t["request_count"]); ok {
			k.RequestCount = int64(rc)
		}
		out = append(out, k)
	}
	return out, nil
}

// FetchGroups 由 /api/pricing 派生分组（04 §3.1 + 第 46 轮实测修正）。
//
// NewAPI 没有独立的分组端点，分组倍率藏在定价响应的顶层 group_ratio 里。
// 可用模型（FR-124）来自每个模型的 enable_groups —— 这是**按分组精确归属**，
// 不是"全量模型都给每个分组"。
//
// ⚠️ 首版把可用模型写成 `asMap(d["model_ratio"])` 的键集合，对真实站点
// 恒为空（新版没有这个映射表），于是 group_models 一行都没写进去，
// 而 sync 报的却是 ok —— 绿色状态、空数据。20 个真实站点全中。
func (a *NewAPIAdapter) FetchGroups(ctx context.Context, s Session) ([]Group, error) {
	m, _, err := a.C.getJSONAuth(ctx, s, "/api/pricing")
	if err != nil {
		return nil, err
	}
	pr, err := parseNewAPIPricing(m)
	if err != nil {
		return nil, err
	}
	if len(pr.GroupRatios) == 0 {
		return nil, fmt.Errorf("/api/pricing 无 group_ratio，无法派生分组")
	}

	now := time.Now()
	out := make([]Group, 0, len(pr.GroupRatios))
	for ref, ratio := range pr.GroupRatios {
		g := Group{
			GroupRef:        ref,
			RateMultiplier:  ratio,
			AvailableModels: pr.GroupModels[ref],
			Meta:            NewAPIMeta("/api/pricing", now),
		}
		// 该分组一个模型都没有：可能是站点只在 usable_group 里列了它、
		// 却没有任何模型 enable 它。标 degraded 让运维看得见，
		// 而不是让 group_models 静默为空（FR-124 的采集目标就是这份清单）。
		if len(g.AvailableModels) == 0 {
			g.Meta.Degraded = true
			g.Meta.MissingFields = []string{"available_models"}
		}
		out = append(out, g)
	}
	return out, nil
}

// FetchPricing 取模型价格（FR-010/012/013）。
//
// 两种计价形态（第 46 轮实测，1525 个真实模型条目）：
//   - quota_type=0 倍率计价：model_ratio 是相对基准价的倍数
//   - quota_type=1 固定价：model_price 是每次调用的绝对价格
//
// 两者**单位不同**（per_1m_token vs per_call），必须逐模型区分 ——
// 混为一谈会让固定价模型的成本估算差若干个数量级（实测 235/1525 是固定价）。
func (a *NewAPIAdapter) FetchPricing(ctx context.Context, s Session) (Pricing, error) {
	m, _, err := a.C.getJSONAuth(ctx, s, "/api/pricing")
	if err != nil {
		return Pricing{}, err
	}
	pr, err := parseNewAPIPricing(m)
	if err != nil {
		return Pricing{}, err
	}
	if len(pr.Models) == 0 {
		return Pricing{}, fmt.Errorf("/api/pricing 未解析出任何模型价格")
	}

	p := Pricing{
		GroupRatios: pr.GroupRatios,
		Meta:        NewAPIMeta("/api/pricing", time.Now()),
	}
	for _, it := range pr.Models {
		p.Models = append(p.Models, it.toModelPrice())
	}
	return p, nil
}

// FetchModelCatalog 取渠道全部可用模型目录（FR-126）。
//
// 与 FetchPricing 同源端点但落库目标不同（channel_model_catalog vs
// price_versions）：目录回答"上游有什么"，价格版本是不可覆盖的计价依据。
// **无 token 上界** —— 那是 models 表（可路由模型）的必填项，目录不需要，
// 这正是目录必须独立于 models 的原因（02 §1.3）。
func (a *NewAPIAdapter) FetchModelCatalog(ctx context.Context, s Session) ([]CatalogModel, error) {
	m, _, err := a.C.getJSONAuth(ctx, s, "/api/pricing")
	if err != nil {
		return nil, err
	}
	pr, err := parseNewAPIPricing(m)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	out := make([]CatalogModel, 0, len(pr.Models))
	for _, it := range pr.Models {
		mp := it.toModelPrice()
		out = append(out, CatalogModel{
			ModelName:   it.Name,
			InputPrice:  mp.InputPrice,
			OutputPrice: mp.OutputPrice,
			BillingUnit: mp.BillingUnit,
			Meta:        NewAPIMeta("/api/pricing", now),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("NewAPI 模型目录为空：supported 能力必须返回数据")
	}
	return out, nil
}

// ── 辅助 ──

// unwrapDataList 取 envelope 里的数组（/api/token 返回 {data:[...]}）。
func unwrapDataList(m map[string]any) any {
	if m == nil {
		return nil
	}
	if d, ok := m["data"]; ok {
		// 有些二开把列表包在 data.items 里
		if dm, ok := d.(map[string]any); ok {
			if items, ok := dm["items"]; ok {
				return items
			}
			if records, ok := dm["records"]; ok {
				return records
			}
			return nil
		}
		return d
	}
	return nil
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

var _ Adapter = (*NewAPIAdapter)(nil)
