package collector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
)

// realNewAPIPricingResponse 是 2026-08 从真实站点抓到的响应形态（已裁剪）。
//
// 特征逐条对应实测：data 是**数组**、group_ratio 在**顶层**、
// 每项带 enable_groups、混有 quota_type=0（倍率）与 =1（按次固定价）。
//
// `vendors` / `vendor_id` / `supported_endpoint_types` 三项的形态取自
// **2026-09-14 实测一个真实 NewAPI 站点**（1394 个模型；站点地址不入库，
// 见 verify/test-public-safety.sh 的「真实验收站点地址」那条扫描）：
//   - vendors 是**数组** `[{id,name,icon}]`，实测 35 项；模型用 vendor_id 指过来
//   - supported_endpoint_types 是字符串数组，实测取值 openai / anthropic /
//     gemini / openai-response / openai-video / image-generation / jina-rerank
//   - 同一批数据里 `owner_by` 1394 个全是空串 —— 所以它不能当供应商的兜底
//
// 特意让 midjourney-relax **不带** vendor_id 与 supported_endpoint_types：
// 老版本站点就没有这两个字段，解析器必须给出空值而不是崩或编一个。
const realNewAPIPricingResponse = `{
  "success": true,
  "pricing_version": "a42d372ccf0b5dd13ecf71203521f9d2",
  "group_ratio": {"default": 1, "vip": 0.8, "专线": 2.6},
  "usable_group": {"default": "默认", "vip": "会员", "专线": "专线通道"},
  "vendors": [
    {"id": 115, "name": "OpenAI", "icon": "OpenAI"},
    {"id": 114, "name": "Anthropic", "icon": "Claude.Color"}
  ],
  "supported_endpoint": {
    "openai": {"path": "/v1/chat/completions", "method": "POST"},
    "anthropic": {"path": "/v1/messages", "method": "POST"}
  },
  "data": [
    {"model_name": "gpt-4o", "quota_type": 0, "model_ratio": 1.25,
     "completion_ratio": 4, "cache_ratio": 0.5, "model_price": 0,
     "vendor_id": 115, "supported_endpoint_types": ["openai", "openai-response"],
     "enable_groups": ["default", "vip"]},
    {"model_name": "claude-opus", "quota_type": 0, "model_ratio": 15,
     "completion_ratio": 5, "model_price": 0,
     "vendor_id": 114, "supported_endpoint_types": ["anthropic", "openai"],
     "enable_groups": ["vip", "专线"]},
    {"model_name": "midjourney-relax", "quota_type": 1, "model_ratio": 0,
     "completion_ratio": 0, "model_price": 0.1,
     "enable_groups": ["default"]}
  ]
}`

// TestParseRealNewAPIPricingShape 用真实响应形态验证解析。
//
// 这个测试的存在本身就是教训：首版按 04 §3.1 记录的 dict 形态写解析器，
// 单测用自造的 dict 数据 —— 全绿，而对 20/20 个真实站点全部取不到价格。
// 测试夹具必须来自真实响应，否则测的只是"我对文档的理解自洽"。
func TestParseRealNewAPIPricingShape(t *testing.T) {
	var raw map[string]any
	mustJSON(t, realNewAPIPricingResponse, &raw)

	pr, err := parseNewAPIPricing(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(pr.Models) != 3 {
		t.Fatalf("模型数 = %d，期望 3", len(pr.Models))
	}
	if pr.Version != "a42d372ccf0b5dd13ecf71203521f9d2" {
		t.Errorf("pricing_version 未解析: %q", pr.Version)
	}
	// group_ratio 在顶层，不在 data 里
	if pr.GroupRatios["vip"] != 0.8 || pr.GroupRatios["专线"] != 2.6 {
		t.Errorf("顶层 group_ratio 未正确解析: %v", pr.GroupRatios)
	}
}

// vendor_id 必须解析成**名字**，端点类型必须原样带出。
//
// 存名字不存 id：id 是站点自己的自增主键，跨站点毫无意义，而界面上"哪些渠道
// 有 Anthropic 的模型"是跨渠道聚合。只存 id 的话，两个站点的 115 会被当成
// 同一家公司 —— 那种错看起来完全正常。
func TestVendorAndEndpointTypes(t *testing.T) {
	var raw map[string]any
	mustJSON(t, realNewAPIPricingResponse, &raw)
	pr, err := parseNewAPIPricing(raw)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]newapiModel{}
	for _, m := range pr.Models {
		byName[m.Name] = m
	}
	if got := byName["gpt-4o"].VendorName; got != "OpenAI" {
		t.Errorf("gpt-4o 的 vendor_id=115 应解析成 OpenAI，实际 %q", got)
	}
	if got := byName["claude-opus"].VendorName; got != "Anthropic" {
		t.Errorf("claude-opus 的 vendor_id=114 应解析成 Anthropic，实际 %q", got)
	}
	// icon 要跟着 name 一起带出来：同一站里多个发行方名共用一个图标
	// （实测四个 Alibaba 系的名字都是 Qwen.Color），前端按名字猜要手写别名表。
	if got := byName["claude-opus"].VendorIcon; got != "Claude.Color" {
		t.Errorf("claude-opus 的图标名应是 Claude.Color，实际 %q", got)
	}
	if got := byName["midjourney-relax"].VendorIcon; got != "" {
		t.Errorf("没有 vendor_id 的模型不该有图标名，实际 %q", got)
	}
	// 反向哨兵：把 vendors 当成 id→name 的 map 解（而它实测是数组）会让
	// 两个都拿不到名字，上面两条会一起红；这条保证"全空"不会被当成通过。
	if byName["gpt-4o"].VendorName == byName["claude-opus"].VendorName {
		t.Error("两个模型解析出了同一个供应商 —— vendors 数组的 id→name 映射没生效")
	}
	if got := byName["gpt-4o"].EndpointTypes; len(got) != 2 ||
		got[0] != "openai" || got[1] != "openai-response" {
		t.Errorf("gpt-4o 的端点类型 = %v，期望 [openai openai-response]（已排序）", got)
	}
	// 老版本站点没有这两个字段：必须是空值，不是崩、也不是编一个默认供应商。
	if got := byName["midjourney-relax"]; got.VendorName != "" || len(got.EndpointTypes) != 0 {
		t.Errorf("缺字段的模型应给空值，实际 vendor=%q endpoints=%v",
			got.VendorName, got.EndpointTypes)
	}
}

// FR-124 的核心：分组 → 可用模型必须**按分组精确归属**。
//
// 首版对真实站点恒为空（group_models 一行都没写），而 sync 报 ok ——
// 绿色状态 + 空数据，比报错更难发现。
func TestGroupModelsFromEnableGroups(t *testing.T) {
	var raw map[string]any
	mustJSON(t, realNewAPIPricingResponse, &raw)
	pr, err := parseNewAPIPricing(raw)
	if err != nil {
		t.Fatal(err)
	}

	// default 组能用 gpt-4o 与 midjourney-relax，**不能**用 claude-opus
	got := pr.GroupModels["default"]
	if len(got) != 2 {
		t.Fatalf("default 组可用模型 = %v，期望 2 个", got)
	}
	for _, m := range got {
		if m == "claude-opus" {
			t.Error("claude-opus 未对 default 组开放，不该出现在其可用模型里 —— " +
				"若把全量模型塞给每个分组，这条就会挂")
		}
	}
	// 专线组只有 claude-opus
	if len(pr.GroupModels["专线"]) != 1 || pr.GroupModels["专线"][0] != "claude-opus" {
		t.Errorf("专线组 = %v，期望仅 claude-opus", pr.GroupModels["专线"])
	}
}

func TestGroupModelsExpandsAllToEveryUsableGroup(t *testing.T) {
	// 来源：NewAPI bdef117 controller/pricing.go 将 enable_groups 含 all 的模型对全部可用组放行：
	// https://github.com/QuantumNous/new-api/blob/bdef117505247769268b209665fb3ad7554c3da7/controller/pricing.go
	var raw map[string]any
	if err := jsonUnmarshal([]byte(`{
		"group_ratio":{"default":1,"vip":0.8},
		"data":[{"model_name":"gpt-global","quota_type":0,"model_ratio":1,
			"enable_groups":["all"]}]
	}`), &raw); err != nil {
		t.Fatal(err)
	}
	pricing, err := parseNewAPIPricing(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{"default", "vip"} {
		models := pricing.GroupModels[group]
		if len(models) != 1 || models[0] != "gpt-global" {
			t.Fatalf("%s 分组模型 = %v，期望包含 gpt-global", group, models)
		}
	}
}

func TestAllGroupExpansionIsSorted(t *testing.T) {
	for attempt := 0; attempt < 100; attempt++ {
		var raw map[string]any
		mustJSON(t, `{
			"group_ratio":{"vip":0.8,"default":1,"premium":2},
			"data":[{"model_name":"gpt-global","quota_type":0,"model_ratio":1,
				"enable_groups":["all"]}]
		}`, &raw)
		pricing, err := parseNewAPIPricing(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !sort.StringsAreSorted(pricing.Models[0].EnableGroups) {
			t.Fatalf("第 %d 次解析的 all 分组顺序不稳定：%v", attempt, pricing.Models[0].EnableGroups)
		}
	}
}

// 两种计价口径必须区分（02 §3：漏区分会让成本比较失去意义）。
func TestBillingUnitDistinguishesQuotaType(t *testing.T) {
	var raw map[string]any
	mustJSON(t, realNewAPIPricingResponse, &raw)
	pr, _ := parseNewAPIPricing(raw)

	byName := map[string]newapiModel{}
	for _, m := range pr.Models {
		byName[m.Name] = m
	}

	// quota_type=0 → 倍率口径
	if u := byName["gpt-4o"].BillingUnit(); u != "per_1m_token" {
		t.Errorf("gpt-4o 计费口径 = %q，期望 per_1m_token", u)
	}
	// quota_type=1 → 按次口径。混为倍率会让 $0.1/次 被当成"倍率 0.1"，
	// 与按 token 计价的模型在同一把尺子上比较，结论必然错
	if u := byName["midjourney-relax"].BillingUnit(); u != "per_call" {
		t.Errorf("midjourney-relax 计费口径 = %q，期望 per_call", u)
	}
}

// completion_ratio 是相对输入的**倍数**，输出价 = 输入价 × 它。
func TestOutputPriceMultipliesCompletionRatio(t *testing.T) {
	var raw map[string]any
	mustJSON(t, realNewAPIPricingResponse, &raw)
	pr, _ := parseNewAPIPricing(raw)

	byName := map[string]newapiModel{}
	for _, m := range pr.Models {
		byName[m.Name] = m
	}

	mp := byName["gpt-4o"].toModelPrice()
	if mp.InputPrice != 1.25 {
		t.Errorf("输入价 = %v，期望 1.25", mp.InputPrice)
	}
	// 1.25 × 4 = 5；若漏乘会得到 4（把倍数当成了绝对值）
	if mp.OutputPrice != 5 {
		t.Errorf("输出价 = %v，期望 5（1.25×4）—— 漏乘 completion_ratio "+
			"会把倍数当成输出价本身", mp.OutputPrice)
	}
	// 1.25 × 0.5 = 0.625
	if mp.CachePrice != 0.625 {
		t.Errorf("缓存价 = %v，期望 0.625", mp.CachePrice)
	}

	// 按次计价：输入输出同价，不该乘任何倍率
	fixed := byName["midjourney-relax"].toModelPrice()
	if fixed.InputPrice != 0.1 || fixed.OutputPrice != 0.1 {
		t.Errorf("按次计价应输入输出同价 0.1，得到 in=%v out=%v",
			fixed.InputPrice, fixed.OutputPrice)
	}
}

// 旧 dict 形态仍要能解析（可能存在未升级的站点），
// 但**不得伪造** enable_groups —— 旧形态确实没有这个信息。
func TestOldDictShapeStillParsesButDoesNotFakeGroupModels(t *testing.T) {
	var raw map[string]any
	mustJSON(t, `{"data":{
		"model_ratio":{"gpt-4":15,"gpt-3.5":1},
		"completion_ratio":{"gpt-4":3},
		"group_ratio":{"default":1,"vip":0.8}}}`, &raw)

	pr, err := parseNewAPIPricing(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(pr.Models) != 2 {
		t.Errorf("旧形态模型数 = %d，期望 2", len(pr.Models))
	}
	if pr.GroupRatios["vip"] != 0.8 {
		t.Errorf("旧形态 group_ratio（在 data 里）未解析: %v", pr.GroupRatios)
	}
	// 关键：旧形态没有分组↔模型关系，**不能**塞全量模型冒充
	if len(pr.GroupModels) != 0 {
		t.Errorf("旧形态没有 enable_groups，不该伪造分组可用模型，得到 %v",
			pr.GroupModels)
	}
}

// 响应结构再变时必须**报错而非静默返回空**。
func TestUnknownShapeFailsLoudly(t *testing.T) {
	var raw map[string]any
	mustJSON(t, `{"data":"这是个字符串"}`, &raw)
	if _, err := parseNewAPIPricing(raw); err == nil {
		t.Fatal("data 既非数组也非对象时应报错 —— 静默返回空会让 sync 报 ok 而无数据")
	}
}

// 端到端：FetchGroups 对真实响应形态必须产出非空可用模型。
func TestFetchGroupsProducesNonEmptyModelsOnRealShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pricing" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(realNewAPIPricingResponse))
	}))
	defer srv.Close()

	ad := NewNewAPIAdapter(NewClient(0))
	ad.C.HC = srv.Client()
	gs, err := ad.FetchGroups(context.Background(), Session{
		Family: FamilyNewAPI, BaseURL: srv.URL, Token: "t",
		UserIDHeader: "New-API-User", ExternalUserID: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(gs) != 3 {
		t.Fatalf("分组数 = %d，期望 3", len(gs))
	}
	for _, g := range gs {
		if len(g.AvailableModels) == 0 {
			t.Errorf("分组 %s 可用模型为空 —— FR-124 采集失败（这正是首版对 "+
				"20/20 真实站点发生的情况，而 sync 报的是 ok）", g.GroupRef)
		}
	}
}

// 分组无可用模型时须标 degraded，而不是静默交出空清单。
func TestEmptyGroupModelsMarkedDegraded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// orphan 组在 group_ratio 里，但没有任何模型 enable 它
		_, _ = w.Write([]byte(`{"group_ratio":{"orphan":1},
			"data":[{"model_name":"m1","quota_type":0,"model_ratio":1,
			         "enable_groups":["other"]}]}`))
	}))
	defer srv.Close()

	ad := NewNewAPIAdapter(NewClient(0))
	ad.C.HC = srv.Client()
	gs, err := ad.FetchGroups(context.Background(), Session{
		Family: FamilyNewAPI, BaseURL: srv.URL, Token: "t",
		UserIDHeader: "New-API-User", ExternalUserID: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	var orphan *Group
	for i := range gs {
		if gs[i].GroupRef == "orphan" {
			orphan = &gs[i]
		}
	}
	if orphan == nil {
		t.Fatal("orphan 组应出现在结果里")
	}
	if !orphan.Meta.Degraded {
		t.Error("无可用模型的分组应标 degraded，让运维看得见而非静默为空")
	}
}

// mustJSON 解析测试夹具。
func mustJSON(t *testing.T, s string, v any) {
	t.Helper()
	if err := jsonUnmarshal([]byte(s), v); err != nil {
		t.Fatalf("测试夹具不是合法 JSON: %v", err)
	}
}
