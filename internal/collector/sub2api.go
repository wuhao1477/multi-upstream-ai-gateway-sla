package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Sub2APIAdapter 是 Sub2API 系适配器（04 §3.2，molifang + hyhawang 实测
// + 源码级 ent schema 解析）。
type Sub2APIAdapter struct{ C *Client }

// NewSub2APIAdapter 构造适配器。
func NewSub2APIAdapter(c *Client) *Sub2APIAdapter {
	if c == nil {
		c = NewClient(200 * time.Millisecond)
	}
	return &Sub2APIAdapter{C: c}
}

func (a *Sub2APIAdapter) Capabilities() CapabilityMap {
	return CapabilityMap{
		CapAccount: Supported,
		CapKeys:    Supported,
		CapGroups:  Degraded,
		// pricing 标 degraded（04 §3.2）：倍率经 groups/available 与分组耦合，
		// **无独立模型价格表** → 目录里逐模型单价会缺失，用 MissingFields 标明。
		CapPricing:      Degraded,
		CapModelCatalog: Degraded,
	}
}

func (a *Sub2APIAdapter) Detect(ctx context.Context, baseURL string) (DetectResult, error) {
	return Detect(ctx, a.C.HC, baseURL)
}

// Authenticate 校验 JWT 可用。
//
// Sub2API 的 JWT 有效期 24h，续期走 Refresh（不变式 S-1：同账号串行）。
// 本方法只验证当前令牌，不做续期 —— 续期由 Authenticator.EnsureFresh 统一
// 调度，那里才有账号锁。
func (a *Sub2APIAdapter) Authenticate(ctx context.Context, cred Credential) (Session, error) {
	if cred.AccessToken == "" {
		return Session{}, fmt.Errorf("Sub2API 需要 JWT（access_token）")
	}
	s := Session{
		ChannelID: cred.ChannelID, Family: FamilySub2API,
		BaseURL: cred.BaseURL, Token: cred.AccessToken,
		ExpiresAt: cred.TokenExpiresAt,
	}
	m, _, err := a.C.getJSONAuth(ctx, s, "/api/v1/auth/me")
	if err != nil {
		return Session{}, err
	}
	d := unwrapData(m)
	if id := asString(d["id"]); id != "" {
		s.ExternalUserID = id
	}
	return s, nil
}

// Refresh 实现 Refresher：POST /api/v1/auth/refresh（04 §5.2）。
//
// ⚠️ 该端点**轮换 refresh_token** —— 并发调用会互相作废。
// 故本方法必须经 Authenticator.EnsureFresh 调用（那里有账号级互斥锁），
// **不要直接调它**。
func (a *Sub2APIAdapter) Refresh(ctx context.Context, cred Credential) (Credential, error) {
	if cred.RefreshToken == "" {
		// **同时标 ErrPrecondition**：这一条在发出任何请求之前就返回了，
		// 与 ErrPrecondition 注释里"凭证没登记"是同一类 —— 登记了但缺
		// refresh_token 仍是纯本地的配置缺口。不标的后果是上层按"上游故障"
		// 处理：返 502 让运维去查别人家站点为什么挂了，并且起算 60s 限流窗口
		// 白等一轮（upstream_api.go 的 422/502 分支注释写了这个理由）。
		// 下面 401 那条**不标** —— 那时请求已经发出去了。
		return cred, fmt.Errorf("%w: 无 refresh_token 可用（需人工重登：%w）",
			ErrPrecondition, ErrNeedsRelogin)
	}
	body, _ := json.Marshal(map[string]string{"refresh_token": cred.RefreshToken})
	url := strings.TrimRight(cred.BaseURL, "/") + "/api/v1/auth/refresh"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return cred, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	if err := a.C.wait(ctx, req.URL.Host); err != nil {
		return cred, err
	}
	resp, err := a.C.HC.Do(req)
	if err != nil {
		return cred, fmt.Errorf("刷新请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))

	if resp.StatusCode == http.StatusUnauthorized {
		// refresh_token 也失效 → 只能人工重登（04 §5.2 第 4 层）
		return cred, fmt.Errorf("%w: refresh_token 已失效", ErrNeedsRelogin)
	}
	if resp.StatusCode != http.StatusOK {
		return cred, fmt.Errorf("刷新返回 %d: %s", resp.StatusCode, snippet(raw))
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return cred, fmt.Errorf("刷新响应非 JSON: %s", snippet(raw))
	}
	d := unwrapData(m)

	next := cred
	if v := asString(d["access_token"]); v != "" {
		next.AccessToken = v
	} else {
		return cred, fmt.Errorf("刷新响应无 access_token: %s", snippet(raw))
	}
	// **必须接住新 refresh_token**：旧的已被这次调用作废，
	// 不更新会让下一次刷新必然失败。
	if v := asString(d["refresh_token"]); v != "" {
		next.RefreshToken = v
	}
	if secs, ok := asFloat(d["expires_in"]); ok && secs > 0 {
		next.TokenExpiresAt = time.Now().Add(time.Duration(secs) * time.Second)
	} else {
		// 响应没给有效期时按官方 24h 兜底，而不是留零值
		//（零值会让 NeedsRefresh 永远返 false，令牌到期后静默 401）
		next.TokenExpiresAt = time.Now().Add(24 * time.Hour)
	}
	return next, nil
}

// FetchAccount 取账号信息（04 §3.2：/api/v1/auth/me 给用户 ID/邮箱/角色）。
//
// ⚠️ 该端点**不给余额** —— Sub2API 的额度在 Key 与分组上（FetchKeys/
// FetchGroups）。故 BalanceUSD 留 0 并标 Degraded + MissingFields，
// 而不是谎报 0 余额（那会让 selector 把渠道判成耗尽）。
func (a *Sub2APIAdapter) FetchAccount(ctx context.Context, s Session) (Account, error) {
	m, _, err := a.C.getJSONAuth(ctx, s, "/api/v1/auth/me")
	if err != nil {
		return Account{}, err
	}
	d := unwrapData(m)
	meta := NewAPIMeta("/api/v1/auth/me", time.Now())
	meta.Degraded = true
	meta.MissingFields = []string{"balance_usd", "used_usd"}
	return Account{
		UserID: asString(d["id"]),
		Meta:   meta,
	}, nil
}

// FetchKeys 取 Key 级额度与限流窗口（04 §3.2）。
func (a *Sub2APIAdapter) FetchKeys(ctx context.Context, s Session) ([]Key, error) {
	m, _, err := a.C.getJSONAuth(ctx, s, "/api/v1/keys")
	if err != nil {
		return nil, err
	}
	items := asSlice(unwrapDataList(m))
	now := time.Now()
	out := make([]Key, 0, len(items))
	for _, it := range items {
		t := asMap(it)
		if t == nil {
			continue
		}
		k := Key{
			KeyRef:   asString(t["id"]),
			GroupRef: asString(t["group_id"]),
			Meta:     NewAPIMeta("/api/v1/keys", now),
		}
		// Sub2API 的额度直接是 USD 浮点（04 §3.4 额度单位表），无需换算
		if q, ok := asFloat(t["quota"]); ok {
			k.RemainQuotaUSD = &q
		}
		if u, ok := asFloat(t["quota_used"]); ok {
			k.UsedQuotaUSD = &u
		}
		if exp := asString(t["expires_at"]); exp != "" {
			if tm, err := time.Parse(time.RFC3339, exp); err == nil {
				k.ExpiredAt = &tm
			}
		}
		if c, ok := asFloat(t["current_concurrency"]); ok {
			k.RateLimit.Concurrency = int(c)
		}
		// 三个滚动窗口（5h/1d/7d）进 payload 时序，不落结构化列
		k.RateLimit.Windows = map[string]WindowUsage{}
		for _, w := range []string{"5h", "1d", "7d"} {
			var wu WindowUsage
			var any bool
			if v, ok := asFloat(t["rate_limit_"+w]); ok {
				wu.LimitUSD, any = v, true
			}
			if v, ok := asFloat(t["usage_"+w]); ok {
				wu.UsageUSD, any = v, true
			}
			if ws := asString(t["window_"+w+"_start"]); ws != "" {
				if tm, err := time.Parse(time.RFC3339, ws); err == nil {
					wu.WindowStart, any = tm, true
				}
			}
			if any {
				k.RateLimit.Windows[w] = wu
			}
		}
		out = append(out, k)
	}
	return out, nil
}

// FetchGroups 取分组倍率、可用模型与限流（04 §3.2 /api/v1/groups/available）。
func (a *Sub2APIAdapter) FetchGroups(ctx context.Context, s Session) ([]Group, error) {
	m, _, err := a.C.getJSONAuth(ctx, s, "/api/v1/groups/available")
	if err != nil {
		return nil, err
	}
	items := asSlice(unwrapDataList(m))
	now := time.Now()
	out := make([]Group, 0, len(items))
	for _, it := range items {
		t := asMap(it)
		if t == nil {
			continue
		}
		g := Group{GroupRef: asString(t["id"]), Meta: NewAPIMeta("/api/v1/groups/available", now)}
		if g.GroupRef == "" {
			g.GroupRef = asString(t["name"])
		}
		if r, ok := asFloat(t["rate_multiplier"]); ok {
			g.RateMultiplier = r
		}
		// 分组可用模型（FR-124）：字段名各站略有差异，逐个尝试
		for _, key := range []string{"available_models", "models", "supported_models"} {
			if lst := asSlice(t[key]); len(lst) > 0 {
				for _, mv := range lst {
					if name := asString(mv); name != "" {
						g.AvailableModels = append(g.AvailableModels, name)
					}
				}
				break
			}
		}
		if len(g.AvailableModels) == 0 {
			g.Meta.Degraded = true
			g.Meta.MissingFields = []string{"available_models"}
		}
		out = append(out, g)
	}
	return out, nil
}

// FetchPricing 取价格 —— **degraded**（04 §3.2）。
//
// Sub2API 无独立模型价格表，倍率与分组耦合在 groups/available。
// 故只能给出分组倍率，逐模型单价缺失 → Meta.Degraded=true +
// MissingFields 说明。**不返回 ErrUnsupported**（04 §3.4bis：
// degraded 是能力声明，方法照常返回数据与 nil）。
func (a *Sub2APIAdapter) FetchPricing(ctx context.Context, s Session) (Pricing, error) {
	groups, err := a.FetchGroups(ctx, s)
	if err != nil {
		return Pricing{}, err
	}
	meta := NewAPIMeta("/api/v1/groups/available", time.Now())
	meta.Degraded = true
	meta.MissingFields = []string{"input_price", "output_price"}
	for _, group := range groups {
		for _, field := range group.Meta.MissingFields {
			if field == "available_models" {
				meta.MissingFields = append(meta.MissingFields, field)
				break
			}
		}
	}

	p := Pricing{GroupRatios: map[string]float64{}, Meta: meta}
	for _, g := range groups {
		p.GroupRatios[g.GroupRef] = g.RateMultiplier
	}
	return p, nil
}

// FetchModelCatalog 从分组的可用模型汇总出渠道级目录（FR-126）。
//
// 单价缺失（见 FetchPricing）→ 目录里 InputPrice/OutputPrice 留 0
// 并标 Degraded。**留 0 而非省略**是因为目录的价格列本就允许空
// （02 §1.3 nonneg_usd 可空），且 Meta 已说明缺什么。
//
// BillingUnit 同样留空：**没有价格就没有口径**。不要"顺手"填
// per_1m_token —— 那会让下游以为这里有个已知单位的 0 价，
// 而 NULL 才如实表达"上游没声明"（02 §1.3bis）。
func (a *Sub2APIAdapter) FetchModelCatalog(ctx context.Context, s Session) ([]CatalogModel, error) {
	groups, err := a.FetchGroups(ctx, s)
	if err != nil {
		return nil, err
	}
	meta := NewAPIMeta("/api/v1/groups/available", time.Now())
	meta.Degraded = true
	meta.MissingFields = []string{"input_price", "output_price"}
	for _, group := range groups {
		for _, field := range group.Meta.MissingFields {
			if field == "available_models" {
				meta.MissingFields = append(meta.MissingFields, field)
				break
			}
		}
	}

	seen := map[string]bool{}
	out := make([]CatalogModel, 0)
	for _, g := range groups {
		for _, name := range g.AvailableModels {
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, CatalogModel{ModelName: name, Meta: meta})
		}
	}
	return out, nil
}

var _ Adapter = (*Sub2APIAdapter)(nil)
var _ Refresher = (*Sub2APIAdapter)(nil)
