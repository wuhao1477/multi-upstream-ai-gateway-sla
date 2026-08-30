package collector

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// allAdapters 返回三家族适配器，供矩阵型断言复用。
func allAdapters(c *Client) map[Family]Adapter {
	return map[Family]Adapter{
		FamilyNewAPI:  NewNewAPIAdapter(c),
		FamilySub2API: NewSub2APIAdapter(c),
		FamilyASXS:    NewASXSAdapter(c),
	}
}

// TestCapabilitiesMatchDocMatrix 断言三家族的能力声明与 04 §3.4 矩阵逐格一致。
//
// 这是 AC-28 的核心判据（"各自 Capabilities() 与 04 §3.4 矩阵一致"）。
// 写成表格是刻意的：文档那张表就是表格，逐格对照才能发现漏改。
func TestCapabilitiesMatchDocMatrix(t *testing.T) {
	// 04 §3.4 三家族能力矩阵总表
	want := map[Family]CapabilityMap{
		FamilyNewAPI: {
			CapAccount: Supported, CapKeys: Supported, CapGroups: Supported,
			CapPricing: Supported, CapModelCatalog: Supported,
			CapSubscriptionQuotas: Unsupported,
		},
		FamilySub2API: {
			CapAccount: Supported, CapKeys: Supported, CapGroups: Supported,
			CapPricing: Degraded, CapModelCatalog: Supported,
			CapSubscriptionQuotas: Unsupported,
		},
		FamilyASXS: {
			CapAccount: Supported, CapKeys: Unsupported, CapGroups: Unsupported,
			CapPricing: Degraded, CapModelCatalog: Degraded,
			CapSubscriptionQuotas: Unsupported,
		},
	}
	for fam, ad := range allAdapters(nil) {
		got := ad.Capabilities()
		exp := want[fam]
		if len(got) != len(exp) {
			t.Errorf("%s 声明 %d 项能力，矩阵有 %d 项", fam, len(got), len(exp))
		}
		for cap, lvl := range exp {
			if got[cap] != lvl {
				t.Errorf("%s.%s = %q，矩阵是 %q（04 §3.4）", fam, cap, got[cap], lvl)
			}
		}
	}
}

// TestUnsupportedDeclarationsReturnErrUnsupported 是 AC-28/AC-38 的"声明与
// 实现一致"判定：声明 unsupported 的能力**必须**返回 ErrUnsupported。
//
// 04 §1 记录过这个坑：此前 Sub2API/ASXS 声明 supported 而实现返回
// ErrUnsupported，自相矛盾，AC-28 必挂。
func TestUnsupportedDeclarationsReturnErrUnsupported(t *testing.T) {
	ctx := context.Background()
	for fam, ad := range allAdapters(nil) {
		caps := ad.Capabilities()
		s := Session{Family: fam, BaseURL: "http://127.0.0.1:1"} // 不可达，确保没真发请求

		check := func(cap Capability, call func() error) {
			t.Helper()
			err := call()
			if caps[cap] == Unsupported {
				if !errors.Is(err, ErrUnsupported) {
					t.Errorf("%s 声明 %s=unsupported，但实现未返回 ErrUnsupported（得到 %v）"+
						" —— AC-28 要求声明与实现一致", fam, cap, err)
				}
				return
			}
			// 声明 supported/degraded 的**不得**返回 ErrUnsupported。
			// 这里请求必然因网络失败，只断言错误类型不是 ErrUnsupported。
			if errors.Is(err, ErrUnsupported) {
				t.Errorf("%s 声明 %s=%s，却返回了 ErrUnsupported —— "+
					"degraded 是能力声明不是运行时状态（04 §3.4bis）", fam, cap, caps[cap])
			}
		}

		check(CapKeys, func() error { _, err := ad.FetchKeys(ctx, s); return err })
		check(CapGroups, func() error { _, err := ad.FetchGroups(ctx, s); return err })
		check(CapSubscriptionQuotas, func() error {
			_, err := ad.FetchSubscriptionQuotas(ctx, s)
			return err
		})
	}
}

// ── NewAPI 适配器 ──

func newAPISite(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 04 §3.1：必须同时带 Authorization 与用户 ID 头，否则 401
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// 只认 Veloera-User —— 模拟一个二开站点改了头名，验证 fan-out
		if r.Header.Get("Veloera-User") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/status":
			_, _ = w.Write([]byte(`{"data":{"quota_per_unit":500000,"turnstile_check":false}}`))
		case "/api/user/self":
			_, _ = w.Write([]byte(`{"data":{"id":42,"quota":1000000,"used_quota":250000}}`))
		case "/api/token":
			_, _ = w.Write([]byte(`{"data":[
				{"id":7,"remain_quota":500000,"used_quota":100000,"unlimited_quota":false,
				 "expired_time":-1,"group":"vip","model_limits_enabled":true,
				 "model_limits":"gpt-4,gpt-3.5","request_count":123},
				{"id":8,"remain_quota":0,"unlimited_quota":true,"expired_time":1900000000,
				 "group":"default","model_limits_enabled":false,"model_limits":"stale-value"}
			]}`))
		case "/api/pricing":
			// ⚠️ 用**真实站点的形态**（2026-08 实测 20/20 站）：data 是模型对象
			// 数组、group_ratio 在顶层。此前这里是 04 §3.1 记录的 dict 形态，
			// 于是单测全绿而真实站点一个价格都采不到 —— 夹具照文档写、
			// 文档又已过时，测试就成了自我印证。旧形态另有专门测试覆盖。
			_, _ = w.Write([]byte(`{"success":true,
				"group_ratio":{"default":1,"vip":0.8},
				"pricing_version":"abc123",
				"data":[
				  {"model_name":"gpt-4","quota_type":0,"model_ratio":15,
				   "completion_ratio":3,"cache_ratio":0.5,
				   "enable_groups":["default","vip"]},
				  {"model_name":"gpt-3.5","quota_type":0,"model_ratio":1,
				   "enable_groups":["default","vip"]},
				  {"model_name":"mj-relax","quota_type":1,"model_price":0.15,
				   "enable_groups":["default"]}
				]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// fan-out 必须能命中被二开改过的头名（04 §3.1）。
func TestNewAPIAuthenticateFanOutFindsHeader(t *testing.T) {
	srv := newAPISite(t)
	defer srv.Close()

	ad := NewNewAPIAdapter(NewClient(0))
	ad.C.HC = srv.Client()
	s, err := ad.Authenticate(context.Background(), Credential{
		Family: FamilyNewAPI, BaseURL: srv.URL,
		AccessToken: "tok", ExternalUserID: "42", QuotaPerUnit: 500000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.UserIDHeader != "Veloera-User" {
		t.Fatalf("fan-out 命中的头名 = %q，期望 Veloera-User", s.UserIDHeader)
	}
}

// 缺 external_user_id 必然 401，应提前拒绝而不是白打七次请求。
func TestNewAPIAuthenticateRequiresUserID(t *testing.T) {
	ad := NewNewAPIAdapter(NewClient(0))
	_, err := ad.Authenticate(context.Background(), Credential{
		Family: FamilyNewAPI, BaseURL: "http://x", AccessToken: "tok",
	})
	if err == nil {
		t.Fatal("缺 external_user_id 应直接失败（04 §3.1：只带 Authorization 必然 401）")
	}
}

// 额度换算：金额 = quota / quota_per_unit（04 §3.1）。
func TestNewAPIFetchAccountNormalizesQuota(t *testing.T) {
	srv := newAPISite(t)
	defer srv.Close()

	ad := NewNewAPIAdapter(NewClient(0))
	ad.C.HC = srv.Client()
	s := Session{
		Family: FamilyNewAPI, BaseURL: srv.URL, Token: "tok",
		UserIDHeader: "Veloera-User", ExternalUserID: "42", QuotaPerUnit: 500000,
	}
	acc, err := ad.FetchAccount(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	// 1000000 / 500000 = 2 美元
	if acc.BalanceUSD != 2 {
		t.Errorf("余额 = %v，期望 2（1000000/500000）", acc.BalanceUSD)
	}
	if acc.UsedUSD != 0.5 {
		t.Errorf("已用 = %v，期望 0.5", acc.UsedUSD)
	}
}

// ⚠️ 缺 quota_per_unit 必须**报错而非猜**：猜 500000 而实际是 1
// 会让余额差 50 万倍，直接导致"有钱判没钱"。
func TestNewAPIFetchAccountRefusesToGuessQuotaPerUnit(t *testing.T) {
	srv := newAPISite(t)
	defer srv.Close()

	ad := NewNewAPIAdapter(NewClient(0))
	ad.C.HC = srv.Client()
	s := Session{
		Family: FamilyNewAPI, BaseURL: srv.URL, Token: "tok",
		UserIDHeader: "Veloera-User", ExternalUserID: "42",
		// QuotaPerUnit 故意为 0
	}
	if _, err := ad.FetchAccount(context.Background(), s); err == nil {
		t.Fatal("缺 quota_per_unit 应报错而不是用默认值猜（差 50 万倍会让余额判断完全错）")
	}
}

// TestNewAPIFetchKeysPagedEnvelope 覆盖 /api/token 的**分页**信封。
//
// ⚠️ 这条是 2026-08-29 补的，补的原因值得记下来：上面 newAPISite 的夹具给的是
// 裸数组 `{"data":[...]}`，于是 unwrapDataList 的 items / records 两个分支
// **零覆盖** —— 而真站点(实测 upstream-a.invalid)给的恰恰是 `data.items`：
//
//	{"data":{"page":1,"page_size":10,"total":1,"items":[{...}]}}
//
// 我写 verify/pick-upstream.mjs 时只认了 records，于是在一个明明有 token 的
// 站上报"账号下没有任何 token"。采集器侧本来是对的(items 在前)，但这份**测试**
// 从没证明过它对 —— 夹具照裸数组写，分页分支就一直没人验。
//
// 这条不违反 CLAUDE.md §1：形态取自真站点实测，不是照我对协议的想象编的。
// 造的是"分页 vs 裸数组"这个**输入变体**，真站点一次只给一种，没法按需切换。
func TestNewAPIFetchKeysPagedEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"items", `{"data":{"page":1,"page_size":10,"total":1,"items":[
			{"id":7,"remain_quota":500000,"group":"vip","expired_time":-1}]}}`},
		{"records", `{"data":{"total":1,"records":[
			{"id":7,"remain_quota":500000,"group":"vip","expired_time":-1}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.URL.Path != "/api/token" {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					_, _ = w.Write([]byte(tc.body))
				}))
			defer srv.Close()

			ad := NewNewAPIAdapter(NewClient(0))
			ad.C.HC = srv.Client()
			keys, err := ad.FetchKeys(context.Background(), Session{
				Family: FamilyNewAPI, BaseURL: srv.URL, Token: "tok",
				UserIDHeader: "New-API-User", ExternalUserID: "42",
				QuotaPerUnit: 500000,
			})
			if err != nil {
				t.Fatal(err)
			}
			// 分页信封没被拆开的话这里会是 0 把 —— 正是 picker 犯的那个错
			if len(keys) != 1 {
				t.Fatalf("Key 数 = %d，期望 1（分页信封未被拆开？）", len(keys))
			}
			if keys[0].KeyRef != "7" {
				t.Errorf("KeyRef = %q，期望 \"7\"", keys[0].KeyRef)
			}
			if keys[0].RemainQuotaUSD != 1 {
				t.Errorf("剩余额度 = %v，期望 1", keys[0].RemainQuotaUSD)
			}
		})
	}
}

func TestNewAPIFetchKeys(t *testing.T) {
	srv := newAPISite(t)
	defer srv.Close()

	ad := NewNewAPIAdapter(NewClient(0))
	ad.C.HC = srv.Client()
	s := Session{
		Family: FamilyNewAPI, BaseURL: srv.URL, Token: "tok",
		UserIDHeader: "Veloera-User", ExternalUserID: "42", QuotaPerUnit: 500000,
	}
	keys, err := ad.FetchKeys(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("Key 数 = %d，期望 2", len(keys))
	}
	k := keys[0]
	if k.RemainQuotaUSD != 1 { // 500000/500000
		t.Errorf("剩余额度 = %v，期望 1", k.RemainQuotaUSD)
	}
	if k.GroupRef != "vip" {
		t.Errorf("分组 = %q", k.GroupRef)
	}
	// expired_time = -1 表示永不过期，不该被当成 1969 年
	if k.ExpiredAt != nil {
		t.Errorf("expired_time=-1 应表示永不过期，得到 %v", k.ExpiredAt)
	}
	if len(k.ModelLimits) != 2 {
		t.Errorf("模型权限 = %v，期望 2 项", k.ModelLimits)
	}
	if k.RequestCount != 123 {
		t.Errorf("请求数 = %d", k.RequestCount)
	}
	// model_limits_enabled=false 时那串值是历史残留，不得当权限用 ——
	// 否则会错误缩小候选集
	if len(keys[1].ModelLimits) != 0 {
		t.Errorf("model_limits_enabled=false 时不该采纳 model_limits，得到 %v",
			keys[1].ModelLimits)
	}
	// 明文不得出现在 KeyRef 里（FR-094）
	if k.KeyRef == "" {
		t.Error("KeyRef 应有脱敏引用值")
	}
}

func TestNewAPIFetchGroupsAndPricing(t *testing.T) {
	srv := newAPISite(t)
	defer srv.Close()

	ad := NewNewAPIAdapter(NewClient(0))
	ad.C.HC = srv.Client()
	s := Session{
		Family: FamilyNewAPI, BaseURL: srv.URL, Token: "tok",
		UserIDHeader: "Veloera-User", ExternalUserID: "42", QuotaPerUnit: 500000,
	}

	groups, err := ad.FetchGroups(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("分组数 = %d，期望 2", len(groups))
	}
	byRef := map[string]Group{}
	for _, g := range groups {
		byRef[g.GroupRef] = g
	}
	if byRef["vip"].RateMultiplier != 0.8 {
		t.Errorf("vip 倍率 = %v，期望 0.8", byRef["vip"].RateMultiplier)
	}
	if len(byRef["vip"].AvailableModels) != 2 {
		t.Errorf("可用模型 = %v，期望 2 个", byRef["vip"].AvailableModels)
	}

	pr, err := ad.FetchPricing(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	var gpt4 *ModelPrice
	for i := range pr.Models {
		if pr.Models[i].ModelName == "gpt-4" {
			gpt4 = &pr.Models[i]
		}
	}
	if gpt4 == nil {
		t.Fatal("未取到 gpt-4 价格")
	}
	if gpt4.InputPrice != 15 {
		t.Errorf("输入价 = %v，期望 15", gpt4.InputPrice)
	}
	// completion_ratio 是相对 model_ratio 的倍数：15 × 3 = 45
	if gpt4.OutputPrice != 45 {
		t.Errorf("输出价 = %v，期望 45（15×3）", gpt4.OutputPrice)
	}
	if gpt4.BillingUnit != "per_1m_token" {
		t.Errorf("计费单位 = %q", gpt4.BillingUnit)
	}
	// NewAPI 声明 pricing=supported，故不该标 degraded
	if pr.Meta.Degraded {
		t.Error("NewAPI 的 pricing 声明 supported，不该标 Degraded")
	}
}

func TestNewAPIModelCatalog(t *testing.T) {
	srv := newAPISite(t)
	defer srv.Close()

	ad := NewNewAPIAdapter(NewClient(0))
	ad.C.HC = srv.Client()
	s := Session{
		Family: FamilyNewAPI, BaseURL: srv.URL, Token: "tok",
		UserIDHeader: "Veloera-User", ExternalUserID: "42", QuotaPerUnit: 500000,
	}
	cat, err := ad.FetchModelCatalog(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat) != 3 {
		t.Fatalf("目录条数 = %d，期望 3", len(cat))
	}

	// 目录必须**逐条带上口径**。此前只断言条数，于是 billing_unit 一直没落库，
	// 而真实站点 15% 的模型是按次计价、数值区间又与倍率重叠
	// （实测按次 0.004~7 vs 倍率 0.01~175）—— 目录里就成了无单位的数字。
	byName := map[string]CatalogModel{}
	for _, c := range cat {
		byName[c.ModelName] = c
	}
	if u := byName["gpt-4"].BillingUnit; u != "per_1m_token" {
		t.Errorf("gpt-4 目录口径 = %q，期望 per_1m_token", u)
	}
	if u := byName["mj-relax"].BillingUnit; u != "per_call" {
		t.Errorf("mj-relax 目录口径 = %q，期望 per_call", u)
	}
	// 按次价取 model_price 而非 model_ratio（后者为 0）
	if p := byName["mj-relax"].InputPrice; p != 0.15 {
		t.Errorf("mj-relax 目录价 = %v，期望 0.15", p)
	}
}

// ── Sub2API 适配器 ──

func sub2apiSite(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/me":
			_, _ = w.Write([]byte(`{"code":0,"data":{"id":"u-1","email":"a@b.c"}}`))
		case "/api/v1/keys":
			_, _ = w.Write([]byte(`{"code":0,"data":[
				{"id":"k-1","group_id":"g-1","quota":12.5,"quota_used":2.5,
				 "current_concurrency":3,"rate_limit_1d":100,"usage_1d":20,
				 "window_1d_start":"2026-08-28T00:00:00Z"}]}`))
		case "/api/v1/groups/available":
			_, _ = w.Write([]byte(`{"code":0,"data":[
				{"id":"g-1","rate_multiplier":0.5,"rpm_limit":60,
				 "subscription_type":"monthly","platform":"openai",
				 "is_exclusive":false,"peak_rate_enabled":true,
				 "peak_rate_multiplier":1.5,"peak_start":"18:00","peak_end":"23:00",
				 "available_models":["gpt-4","claude-3"]}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestSub2APIFetchKeysUsesUSDDirectly(t *testing.T) {
	srv := sub2apiSite(t)
	defer srv.Close()

	ad := NewSub2APIAdapter(NewClient(0))
	ad.C.HC = srv.Client()
	s := Session{Family: FamilySub2API, BaseURL: srv.URL, Token: "jwt"}
	keys, err := ad.FetchKeys(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("Key 数 = %d", len(keys))
	}
	k := keys[0]
	// Sub2API 额度已是 USD 浮点，**不需换算**（04 §3.4 额度单位表）
	if k.RemainQuotaUSD != 12.5 {
		t.Errorf("剩余 = %v，期望 12.5（USD 浮点直接用）", k.RemainQuotaUSD)
	}
	if k.RateLimit.Concurrency != 3 {
		t.Errorf("并发 = %d", k.RateLimit.Concurrency)
	}
	w, ok := k.RateLimit.Windows["1d"]
	if !ok {
		t.Fatal("缺 1d 窗口用量")
	}
	if w.LimitUSD != 100 || w.UsageUSD != 20 {
		t.Errorf("1d 窗口 = %+v", w)
	}
}

func TestSub2APIFetchGroups(t *testing.T) {
	srv := sub2apiSite(t)
	defer srv.Close()

	ad := NewSub2APIAdapter(NewClient(0))
	ad.C.HC = srv.Client()
	s := Session{Family: FamilySub2API, BaseURL: srv.URL, Token: "jwt"}
	groups, err := ad.FetchGroups(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	g := groups[0]
	if g.RateMultiplier != 0.5 || g.RPMLimit != 60 {
		t.Errorf("倍率/RPM = %v/%d", g.RateMultiplier, g.RPMLimit)
	}
	if len(g.AvailableModels) != 2 {
		t.Errorf("可用模型 = %v", g.AvailableModels)
	}
	// 高峰倍率采到但只进 payload（ISSUE-005 §3.1），此处验证确实采到了
	if !g.PeakEnabled || g.PeakMultiplier != 1.5 {
		t.Errorf("高峰字段未采到: enabled=%v mult=%v", g.PeakEnabled, g.PeakMultiplier)
	}
}

// Sub2API 声明 pricing=degraded：必须返回数据 + nil，并标明缺什么。
func TestSub2APIPricingIsDegradedNotUnsupported(t *testing.T) {
	srv := sub2apiSite(t)
	defer srv.Close()

	ad := NewSub2APIAdapter(NewClient(0))
	ad.C.HC = srv.Client()
	s := Session{Family: FamilySub2API, BaseURL: srv.URL, Token: "jwt"}

	pr, err := ad.FetchPricing(context.Background(), s)
	if err != nil {
		t.Fatalf("degraded 能力应返回 nil error（04 §3.4bis）: %v", err)
	}
	if !pr.Meta.Degraded {
		t.Error("应标 Meta.Degraded=true")
	}
	if len(pr.Meta.MissingFields) == 0 {
		t.Error("应用 MissingFields 说明缺哪些字段（供 inventory 的待补录计数）")
	}
	if len(pr.GroupRatios) == 0 {
		t.Error("degraded 不等于没数据：分组倍率仍应采到")
	}
}

// FetchAccount 拿不到余额时必须标 degraded，**不得谎报 0 余额** ——
// 那会让 selector 把渠道判成耗尽。
func TestSub2APIAccountDoesNotFakeZeroBalance(t *testing.T) {
	srv := sub2apiSite(t)
	defer srv.Close()

	ad := NewSub2APIAdapter(NewClient(0))
	ad.C.HC = srv.Client()
	s := Session{Family: FamilySub2API, BaseURL: srv.URL, Token: "jwt"}
	acc, err := ad.FetchAccount(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !acc.Meta.Degraded {
		t.Error("Sub2API 的 /auth/me 不给余额，必须标 Degraded 而非静默留 0")
	}
	found := false
	for _, f := range acc.Meta.MissingFields {
		if f == "balance_usd" {
			found = true
		}
	}
	if !found {
		t.Errorf("MissingFields 应含 balance_usd，得到 %v", acc.Meta.MissingFields)
	}
}

// Refresh 必须接住轮换后的 refresh_token —— 旧的已被作废（04 §5.2）。
func TestSub2APIRefreshCapturesRotatedToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/refresh" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"access_token":"new-a",
			"refresh_token":"new-r","expires_in":86400}}`))
	}))
	defer srv.Close()

	ad := NewSub2APIAdapter(NewClient(0))
	ad.C.HC = srv.Client()
	got, err := ad.Refresh(context.Background(), Credential{
		Family: FamilySub2API, BaseURL: srv.URL, RefreshToken: "old-r",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "new-a" {
		t.Errorf("access_token = %q", got.AccessToken)
	}
	if got.RefreshToken != "new-r" {
		t.Fatalf("refresh_token = %q，必须接住轮换后的值（旧的已被作废，"+
			"不更新会让下次刷新必然失败）", got.RefreshToken)
	}
	if got.TokenExpiresAt.Before(time.Now().Add(23 * time.Hour)) {
		t.Errorf("到期时间未按 expires_in 设置: %v", got.TokenExpiresAt)
	}
}

// 响应没给 expires_in 时应按 24h 兜底，而不是留零值
// （零值会让 NeedsRefresh 永远返 false，令牌到期后静默 401）。
func TestSub2APIRefreshDefaultsExpiry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"access_token":"a","refresh_token":"r"}}`))
	}))
	defer srv.Close()

	ad := NewSub2APIAdapter(NewClient(0))
	ad.C.HC = srv.Client()
	got, err := ad.Refresh(context.Background(), Credential{
		Family: FamilySub2API, BaseURL: srv.URL, RefreshToken: "old",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.TokenExpiresAt.IsZero() {
		t.Fatal("缺 expires_in 时应按 24h 兜底——留零值会让 NeedsRefresh 恒 false")
	}
}

func TestSub2APIRefreshWithoutTokenNeedsRelogin(t *testing.T) {
	ad := NewSub2APIAdapter(NewClient(0))
	_, err := ad.Refresh(context.Background(), Credential{Family: FamilySub2API})
	if !errors.Is(err, ErrNeedsRelogin) {
		t.Fatalf("无 refresh_token 应返回 ErrNeedsRelogin，得到 %v", err)
	}
}

// ── ASXS 适配器 ──

func TestASXSBalanceHandlesStringNumber(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/me/balance":
			// ⚠️ balanceUsd 是**字符串**（04 §3.3 实测）
			_, _ = w.Write([]byte(`{"data":{"balanceUsd":"90.50","usedMicros":1500000}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	ad := NewASXSAdapter(NewClient(0))
	ad.C.HC = srv.Client()
	acc, err := ad.FetchAccount(context.Background(),
		Session{Family: FamilyASXS, BaseURL: srv.URL, Token: "jwt"})
	if err != nil {
		t.Fatal(err)
	}
	if acc.BalanceUSD != 90.50 {
		t.Fatalf("余额 = %v，期望 90.5 —— balanceUsd 是字符串，"+
			"用 float64 断言会静默得到 0，把有钱的渠道判成耗尽", acc.BalanceUSD)
	}
	// usedMicros 按 1e6 换算（04 §3.3）
	if acc.UsedUSD != 1.5 {
		t.Errorf("已用 = %v，期望 1.5（1500000/1e6）", acc.UsedUSD)
	}
}

// 余额字段全缺时必须报错，不能静默返回 0 余额。
func TestASXSBalanceRefusesToFakeZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"unrelated":1}}`))
	}))
	defer srv.Close()

	ad := NewASXSAdapter(NewClient(0))
	ad.C.HC = srv.Client()
	if _, err := ad.FetchAccount(context.Background(),
		Session{Family: FamilyASXS, BaseURL: srv.URL, Token: "jwt"}); err == nil {
		t.Fatal("余额字段全缺应报错，不能静默返回 0（那会被判成余额耗尽）")
	}
}

// ASXS 无 refresh 路径，无账密时必须要求人工重登（04 §5.3）。
func TestASXSRefreshRequiresCredentials(t *testing.T) {
	ad := NewASXSAdapter(NewClient(0))
	_, err := ad.Refresh(context.Background(), Credential{Family: FamilyASXS})
	if !errors.Is(err, ErrNeedsRelogin) {
		t.Fatalf("无账密应返回 ErrNeedsRelogin，得到 %v", err)
	}
}

func TestASXSReloginSets7DayExpiry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/manage/auth/login" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"token":"fresh-jwt"}}`))
	}))
	defer srv.Close()

	ad := NewASXSAdapter(NewClient(0))
	ad.C.HC = srv.Client()
	got, err := ad.Refresh(context.Background(), Credential{
		Family: FamilyASXS, BaseURL: srv.URL,
		Username: "u", Password: "p",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "fresh-jwt" {
		t.Errorf("token = %q", got.AccessToken)
	}
	// 04 §3.3 实测：ASXS 的 JWT 固定 168 小时
	wantMin := time.Now().Add(167 * time.Hour)
	if got.TokenExpiresAt.Before(wantMin) {
		t.Errorf("到期时间 = %v，期望约 7 天后", got.TokenExpiresAt)
	}
}

// ── 限速 ──

// 同 host 的连续请求必须被限速隔开（04 §6：避免触发风控）。
func TestClientRateLimitsPerHost(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()

	c := NewClient(80 * time.Millisecond)
	c.HC = srv.Client()
	s := Session{Family: FamilySub2API, BaseURL: srv.URL, Token: "t"}

	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, _, err := c.getJSONAuth(context.Background(), s, "/x"); err != nil {
			t.Fatal(err)
		}
	}
	// 3 次请求至少间隔 2 个 interval
	if el := time.Since(start); el < 160*time.Millisecond {
		t.Errorf("3 次请求耗时 %v，期望 ≥160ms（限速未生效会触发上游风控）", el)
	}
	if hits != 3 {
		t.Errorf("实际请求 %d 次", hits)
	}
}

// 限速表不得随"见过多少 host"单调增长。
//
// 采集器是长驻进程，65 个渠道跨若干站点，加上跳转/多域名，`last` 只增不减
// 意味着进程活多久它就多大。过期条目（占位时刻早于 now-MinInterval）再取出来
// 算 sleep 也必然 ≤0，对限速判定毫无作用。
//
// 直接调 wait 而不打 HTTP：要验的是表的收缩，跟传输无关。
// 这里的 sleep 是**被测语义本身要求的**（条目按墙钟过期），不是"睡一会儿盼着
// 异步完成"——且慢机器只会让表更小，断言方向上不可能假红。
func TestRateLimitTableDoesNotGrowUnbounded(t *testing.T) {
	const interval = 20 * time.Millisecond
	c := NewClient(interval)
	ctx := context.Background()

	for i := 0; i < 50; i++ {
		if err := c.wait(ctx, fmt.Sprintf("host-%d.example", i)); err != nil {
			t.Fatal(err)
		}
	}
	c.mu.Lock()
	grew := len(c.last)
	c.mu.Unlock()
	// 先确认真的装进去了 —— 否则后面的"变小了"可能只是因为一直是空表
	if grew < 2 {
		t.Fatalf("50 个 host 只记下 %d 条，这个用例没在测它想测的东西", grew)
	}

	time.Sleep(interval + 10*time.Millisecond) // 让上面那批全部过期
	if err := c.wait(ctx, "trigger.example"); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	after := len(c.last)
	c.mu.Unlock()
	// 只应剩刚写进去的那条（慢机器上可能更少都不会，因为它刚写；更多则说明没清）
	if after > 1 {
		t.Errorf("过期后表里仍有 %d 条（清理前 %d 条），限速表在无界增长", after, grew)
	}
}

// 401 必须可被识别，供调用方触发续期或人工重登。
func TestUnauthorizedIsDistinguishable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewClient(0)
	c.HC = srv.Client()
	_, _, err := c.getJSONAuth(context.Background(),
		Session{Family: FamilySub2API, BaseURL: srv.URL, Token: "t"}, "/x")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("401 应返回 ErrUnauthorized，得到 %v", err)
	}
}
