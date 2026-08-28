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
		// NewAPI 系**无订阅对象**（04 §3.1）
		CapSubscriptionQuotas: Unsupported,
	}
}

func (a *NewAPIAdapter) Detect(ctx context.Context, baseURL string) (DetectResult, error) {
	return Detect(ctx, a.C.HC, baseURL)
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
			k.RemainQuotaUSD = remain / qpu
		}
		if used, ok := asFloat(t["used_quota"]); ok {
			k.UsedQuotaUSD = used / qpu
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

// FetchGroups 由 /api/pricing 的 group_ratio 派生分组（04 §3.1）。
//
// NewAPI 没有独立的分组端点，分组倍率藏在公开的定价响应里。
// 可用模型则由 model_ratio 的键集合给出 —— 对 NewAPI 而言"分组能用哪些模型"
// 需要看 model_ratio 与该分组的启用关系，一期取全量模型作为该分组可用集合
// （NewAPI 的分组主要影响倍率而非可见性）。
func (a *NewAPIAdapter) FetchGroups(ctx context.Context, s Session) ([]Group, error) {
	m, _, err := a.C.getJSONAuth(ctx, s, "/api/pricing")
	if err != nil {
		return nil, err
	}
	d := unwrapData(m)
	ratios := asMap(d["group_ratio"])
	if len(ratios) == 0 {
		return nil, fmt.Errorf("/api/pricing 无 group_ratio，无法派生分组")
	}

	models := make([]string, 0, len(asMap(d["model_ratio"])))
	for name := range asMap(d["model_ratio"]) {
		models = append(models, name)
	}

	now := time.Now()
	out := make([]Group, 0, len(ratios))
	for ref, v := range ratios {
		r, _ := asFloat(v)
		out = append(out, Group{
			GroupRef:        ref,
			RateMultiplier:  r,
			AvailableModels: models,
			Meta:            NewAPIMeta("/api/pricing", now),
		})
	}
	return out, nil
}

// FetchSubscriptionQuotas ⏭ NewAPI 系无订阅对象（04 §3.1）。
func (a *NewAPIAdapter) FetchSubscriptionQuotas(context.Context, Session) ([]SubscriptionQuota, error) {
	return nil, ErrUnsupported
}

// FetchPricing 取模型价格（FR-010/012/013）。
//
// NewAPI 的 /api/pricing 是**倍率**而非绝对价格：model_ratio 是相对基准价的
// 倍数。一期按"倍率即价格数值"落库并在 billing_unit 标明口径 ——
// 绝对价格需要基准价，而那是站点私有配置、公开端点不给。
// 这是已知的精度局限，不是遗漏（FR-011：采不到的按人工录入补）。
func (a *NewAPIAdapter) FetchPricing(ctx context.Context, s Session) (Pricing, error) {
	m, _, err := a.C.getJSONAuth(ctx, s, "/api/pricing")
	if err != nil {
		return Pricing{}, err
	}
	d := unwrapData(m)
	modelRatio := asMap(d["model_ratio"])
	if len(modelRatio) == 0 {
		return Pricing{}, fmt.Errorf("/api/pricing 无 model_ratio")
	}
	completion := asMap(d["completion_ratio"])
	cache := asMap(d["cache_ratio"])

	p := Pricing{
		GroupRatios: map[string]float64{},
		Meta:        NewAPIMeta("/api/pricing", time.Now()),
	}
	for name, v := range modelRatio {
		in, _ := asFloat(v)
		out := in
		if c, ok := asFloat(completion[name]); ok {
			// completion_ratio 是相对 model_ratio 的倍数
			out = in * c
		}
		mp := ModelPrice{
			ModelName: name, InputPrice: in, OutputPrice: out,
			// NewAPI 的倍率以"每 1M token 的基准价倍数"表达
			BillingUnit: "per_1m_token",
		}
		if cr, ok := asFloat(cache[name]); ok {
			mp.CachePrice = in * cr
		}
		p.Models = append(p.Models, mp)
	}
	for ref, v := range asMap(d["group_ratio"]) {
		r, _ := asFloat(v)
		p.GroupRatios[ref] = r
	}
	return p, nil
}

// FetchModelCatalog 取渠道全部可用模型目录（FR-126）。
//
// 与 FetchPricing 同源端点但落库目标不同（channel_model_catalog vs
// price_versions）：目录回答"上游有什么"，价格版本是不可覆盖的计价依据。
func (a *NewAPIAdapter) FetchModelCatalog(ctx context.Context, s Session) ([]CatalogModel, error) {
	pr, err := a.FetchPricing(ctx, s)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]CatalogModel, 0, len(pr.Models))
	for _, mp := range pr.Models {
		out = append(out, CatalogModel{
			ModelName:   mp.ModelName,
			InputPrice:  mp.InputPrice,
			OutputPrice: mp.OutputPrice,
			Meta:        NewAPIMeta("/api/pricing", now),
		})
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
