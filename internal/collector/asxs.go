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

// ASXSAdapter 是 ASXS（闭源 AMP Manager）适配器（04 §3.3，upstream-f.invalid 已验证）。
//
// ⚠️ **本适配器专属，不可复用、不作探测基准**（04 总原则）：其命名空间
// （/api/me/*）、额度单位（micros）、JWT claim（iss=ampmanager）与
// NewAPI/Sub2API 零重叠。
type ASXSAdapter struct{ C *Client }

// NewASXSAdapter 构造适配器。
func NewASXSAdapter(c *Client) *ASXSAdapter {
	if c == nil {
		c = NewClient(200 * time.Millisecond)
	}
	return &ASXSAdapter{C: c}
}

// microsPerUSD 是 ASXS 的额度单位换算（04 §3.3：limitMicros:90000000 = $90）。
const microsPerUSD = 1e6

func (a *ASXSAdapter) Capabilities() CapabilityMap {
	return CapabilityMap{
		CapAccount: Supported,
		// ASXS 无独立 Key / 分组管理视图（04 §3.3）
		CapKeys:   Unsupported,
		CapGroups: Unsupported,
		// 价格并入套餐 products，非独立价格表 → degraded
		CapPricing:      Degraded,
		CapModelCatalog: Degraded,
		// ⏭ P4
		CapSubscriptionQuotas: Unsupported,
	}
}

func (a *ASXSAdapter) Detect(ctx context.Context, baseURL string) (DetectResult, error) {
	return Detect(ctx, a.C.HC, baseURL)
}

// Authenticate 校验 7 天 JWT 可用。
//
// ASXS **无任何静默续期**（04 §3.3 实测：无 refresh_token、无 refresh claim、
// /api/me/refresh 等全 404）。唯一续法是账密重登 → 见 Refresh。
func (a *ASXSAdapter) Authenticate(ctx context.Context, cred Credential) (Session, error) {
	if cred.AccessToken == "" {
		// 无令牌但有账密 → 直接登录换一个（首次接入的正常路径）
		if cred.Username != "" && cred.Password != "" {
			fresh, err := a.Refresh(ctx, cred)
			if err != nil {
				return Session{}, err
			}
			cred = fresh
		} else {
			return Session{}, fmt.Errorf(
				"ASXS 需要 JWT 或账号密码（无 refresh 路径，04 §3.3）")
		}
	}
	s := Session{
		ChannelID: cred.ChannelID, Family: FamilyASXS,
		BaseURL: cred.BaseURL, Token: cred.AccessToken,
		ExpiresAt: cred.TokenExpiresAt,
	}
	m, _, err := a.C.getJSONAuth(ctx, s, "/api/me/profile")
	if err != nil {
		return Session{}, err
	}
	if id := asString(unwrapData(m)["id"]); id != "" {
		s.ExternalUserID = id
	}
	return s, nil
}

// Refresh 账密重登换新 7 天 JWT（04 §5.3）。
//
// 这是 ASXS 唯一的续期路径 —— 它没有 refresh_token。
// 好处是 7 天有效期让重登频率极低（约每周一次），风控压力小；
// 代价是**必须持有账号密码**（闭源平台固有约束，一期明文存储 FR-113）。
func (a *ASXSAdapter) Refresh(ctx context.Context, cred Credential) (Credential, error) {
	if cred.Username == "" || cred.Password == "" {
		return cred, fmt.Errorf(
			"%w: ASXS 无 refresh 路径，续期必须有账号密码（04 §5.3）", ErrNeedsRelogin)
	}
	body, _ := json.Marshal(map[string]string{
		"username": cred.Username, "password": cred.Password,
	})
	url := strings.TrimRight(cred.BaseURL, "/") + "/api/manage/auth/login"
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
		return cred, fmt.Errorf("重登请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))

	if resp.StatusCode != http.StatusOK {
		return cred, fmt.Errorf("%w: 重登返回 %d: %s",
			ErrNeedsRelogin, resp.StatusCode, snippet(raw))
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return cred, fmt.Errorf("重登响应非 JSON: %s", snippet(raw))
	}
	d := unwrapData(m)

	next := cred
	// 字段名各版本略异，逐个尝试
	for _, k := range []string{"token", "access_token", "jwt"} {
		if v := asString(d[k]); v != "" {
			next.AccessToken = v
			break
		}
	}
	if next.AccessToken == "" {
		return cred, fmt.Errorf("重登响应无 token: %s", snippet(raw))
	}
	// ASXS 的 JWT 固定 168 小时（04 §3.3 实测）
	next.TokenExpiresAt = time.Now().Add(168 * time.Hour)
	return next, nil
}

// FetchAccount 取余额（04 §3.3 /api/me/balance）。
//
// ⚠️ balanceUsd **是字符串**（04 §3.3）—— asFloat 必须容忍字符串数字，
// 否则会静默得到 0，把一个有钱的渠道判成耗尽。
func (a *ASXSAdapter) FetchAccount(ctx context.Context, s Session) (Account, error) {
	m, _, err := a.C.getJSONAuth(ctx, s, "/api/me/balance")
	if err != nil {
		return Account{}, err
	}
	d := unwrapData(m)
	acc := Account{
		UserID: s.ExternalUserID,
		Meta:   NewAPIMeta("/api/me/balance", time.Now()),
	}
	if v, ok := asFloat(d["balanceUsd"]); ok {
		acc.BalanceUSD = v
	} else if v, ok := asFloat(d["limitMicros"]); ok {
		// 退路：只给 micros 时按 1e6 换算（04 §3.3 额度单位）
		acc.BalanceUSD = v / microsPerUSD
	} else {
		return Account{}, fmt.Errorf(
			"/api/me/balance 既无 balanceUsd 也无 limitMicros，无法确定余额")
	}
	if v, ok := asFloat(d["usedMicros"]); ok {
		acc.UsedUSD = v / microsPerUSD
	}
	return acc, nil
}

// FetchKeys ⏭ ASXS 无独立 Key 管理视图（04 §3.3/§3.4）。
func (a *ASXSAdapter) FetchKeys(context.Context, Session) ([]Key, error) {
	return nil, ErrUnsupported
}

// FetchGroups ⏭ ASXS 无分组概念（04 §3.3/§3.4）。
func (a *ASXSAdapter) FetchGroups(context.Context, Session) ([]Group, error) {
	return nil, ErrUnsupported
}

// FetchSubscriptionQuotas ⏭ P4。
//
// ⚠️ ASXS 的订阅数据其实**最完整**（04 §3.3：/api/me/billing/state 覆盖
// FR-033 几乎全部字段），但订阅制整体推迟到 P4，故此处与其它站型一致返回
// ErrUnsupported —— 保持 Capabilities 声明与实现一致（AC-28）。
func (a *ASXSAdapter) FetchSubscriptionQuotas(context.Context, Session) ([]SubscriptionQuota, error) {
	return nil, ErrUnsupported
}

// FetchPricing 从套餐 products 取价格 —— **degraded**（04 §3.3）。
//
// ASXS 把价格并入套餐而非独立模型价格表，故只能取到套餐维度，
// 逐模型单价缺失。**不返回 ErrUnsupported**（04 §3.4bis）。
func (a *ASXSAdapter) FetchPricing(ctx context.Context, s Session) (Pricing, error) {
	m, _, err := a.C.getJSONAuth(ctx, s, "/api/me/purchase/products")
	if err != nil {
		return Pricing{}, err
	}
	meta := NewAPIMeta("/api/me/purchase/products", time.Now())
	meta.Degraded = true
	meta.MissingFields = []string{"input_price", "output_price", "per_model_pricing"}

	p := Pricing{GroupRatios: map[string]float64{}, Meta: meta}
	for _, it := range asSlice(unwrapDataList(m)) {
		t := asMap(it)
		if t == nil {
			continue
		}
		name := asString(t["subscriptionPlanName"])
		if name == "" {
			name = asString(t["name"])
		}
		if name == "" {
			continue
		}
		// priceCnyCent 一期按 1:1 归一美元（FR-018 币种仅名称）
		if cent, ok := asFloat(t["priceCnyCent"]); ok {
			p.GroupRatios[name] = cent / 100
		}
	}
	return p, nil
}

// FetchModelCatalog 取套餐可用模型 —— **degraded**（04 §3.3）。
//
// ASXS 无独立目录端点，模型名藏在套餐里；单价一律缺失，
// 故 BillingUnit 也留空（无价即无口径，同 Sub2API 的理由）。
func (a *ASXSAdapter) FetchModelCatalog(ctx context.Context, s Session) ([]CatalogModel, error) {
	m, _, err := a.C.getJSONAuth(ctx, s, "/api/me/purchase/products")
	if err != nil {
		return nil, err
	}
	meta := NewAPIMeta("/api/me/purchase/products", time.Now())
	meta.Degraded = true
	meta.MissingFields = []string{"input_price", "output_price"}

	seen := map[string]bool{}
	out := make([]CatalogModel, 0)
	for _, it := range asSlice(unwrapDataList(m)) {
		t := asMap(it)
		for _, key := range []string{"models", "supportedModels", "availableModels"} {
			for _, mv := range asSlice(t[key]) {
				name := asString(mv)
				if name == "" || seen[name] {
					continue
				}
				seen[name] = true
				out = append(out, CatalogModel{ModelName: name, Meta: meta})
			}
		}
	}
	return out, nil
}

var _ Adapter = (*ASXSAdapter)(nil)
var _ Refresher = (*ASXSAdapter)(nil)
