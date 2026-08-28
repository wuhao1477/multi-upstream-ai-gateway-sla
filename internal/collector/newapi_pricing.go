package collector

import (
	"encoding/json"
	"fmt"
	"sort"
)

// newapiPricing 是 /api/pricing 的解析结果。
//
// 三个方法（FetchPricing / FetchGroups / FetchModelCatalog）打的是同一个端点，
// 共享本结构避免三份各写一遍解析 —— 首版就是那样写的，于是 group 的解析
// 用错了字段而 pricing 没有，两处对同一份响应有不同理解。
type newapiPricing struct {
	// Models 是逐模型条目。
	Models []newapiModel
	// GroupRatios 是分组倍率，来自响应**顶层** group_ratio。
	GroupRatios map[string]float64
	// GroupModels 是分组 → 可用模型，由每个模型的 enable_groups 反转得出。
	GroupModels map[string][]string
	// Version 是站点侧的定价版本指纹（pricing_version），可用于判断价格是否变过。
	Version string
}

// newapiModel 是一个模型的定价条目。
type newapiModel struct {
	Name string
	// QuotaType 0 = 按 token 倍率计价，1 = 按次固定价。
	// **两者单位不同**，混在一起会让成本计算错得离谱（见 BillingUnit）。
	QuotaType int
	// ModelRatio 是相对基准价的输入倍率（quota_type=0 时有效）。
	ModelRatio float64
	// CompletionRatio 是输出相对输入的倍率。
	CompletionRatio float64
	// CacheRatio 是缓存命中相对输入的倍率（并非所有模型都有）。
	CacheRatio float64
	HasCache   bool
	// ModelPrice 是按次固定价（quota_type=1 时有效），单位美元/次。
	ModelPrice float64
	// EnableGroups 是能用该模型的分组列表 —— FR-124 的真正数据源。
	EnableGroups []string
}

// BillingUnit 返回该条目的计费口径。
//
// ⚠️ 这个区分不可省略（02 §3 与 #7 的 billing_unit 守卫）：
//   - quota_type=0：值是**倍率**，需乘站点基准价才是金额，口径 per_1m_token
//   - quota_type=1：值是**每次调用的绝对美元价**，口径 per_call
//
// 实测 6 个站点 1525 个模型里有 235 个是 quota_type=1。若把它们也当倍率，
// 一个 $0.1/次 的模型会被当成"倍率 0.1"参与成本排序 —— 与按 token 计价的
// 模型放在同一把尺子上比较，结论必然错。
func (m newapiModel) BillingUnit() string {
	if m.QuotaType == 1 {
		return "per_call"
	}
	return "per_1m_token"
}

// parseNewAPIPricing 解析 /api/pricing。
//
// ⚠️ **响应形态已变**（2026-08 实测 20 个真实站点）：
//
//	旧（04 §3.1 记录的形态）：data 是 dict，含 model_ratio / completion_ratio
//	                          / cache_ratio / group_ratio 四张映射表
//	新（20/20 站点的实际形态）：data 是**模型对象数组**，每项含 model_name /
//	                          model_ratio / completion_ratio / enable_groups；
//	                          group_ratio 在响应**顶层**而非 data 里
//
// 实测结果：dict 形态 **0 站**，list 形态 20 站。也就是说按文档写的解析器
// 对当前 NewAPI 版本**全部站点都取不到价格**。故本函数以 list 为主路径，
// 同时保留 dict 分支 —— 旧版本站点仍可能存在，而两种形态可无歧义区分。
func parseNewAPIPricing(raw map[string]any) (*newapiPricing, error) {
	out := &newapiPricing{
		GroupRatios: map[string]float64{},
		GroupModels: map[string][]string{},
		Version:     asString(raw["pricing_version"]),
	}

	// group_ratio 在顶层（新形态）；旧形态在 data 里，两处都看一下。
	// 顶层优先：新形态是当前唯一实际存在的形态。
	ratios := asMap(raw["group_ratio"])
	if len(ratios) == 0 {
		ratios = asMap(dig(raw, "data", "group_ratio"))
	}
	for ref, v := range ratios {
		if r, ok := asFloat(v); ok {
			out.GroupRatios[ref] = r
		}
	}

	switch data := raw["data"].(type) {
	case []any:
		// ── 新形态：模型对象数组 ──
		for _, it := range data {
			m := asMap(it)
			if m == nil {
				continue
			}
			name := asString(m["model_name"])
			if name == "" {
				continue
			}
			mod := newapiModel{Name: name}
			if qt, ok := asFloat(m["quota_type"]); ok {
				mod.QuotaType = int(qt)
			}
			mod.ModelRatio, _ = asFloat(m["model_ratio"])
			mod.CompletionRatio, _ = asFloat(m["completion_ratio"])
			mod.ModelPrice, _ = asFloat(m["model_price"])
			if cr, ok := asFloat(m["cache_ratio"]); ok {
				mod.CacheRatio, mod.HasCache = cr, true
			}
			for _, g := range asSlice(m["enable_groups"]) {
				if ref := asString(g); ref != "" {
					mod.EnableGroups = append(mod.EnableGroups, ref)
					out.GroupModels[ref] = append(out.GroupModels[ref], name)
				}
			}
			out.Models = append(out.Models, mod)
		}

	case map[string]any:
		// ── 旧形态：四张映射表 ──
		modelRatio := asMap(data["model_ratio"])
		completion := asMap(data["completion_ratio"])
		cache := asMap(data["cache_ratio"])
		for name, v := range modelRatio {
			mod := newapiModel{Name: name}
			mod.ModelRatio, _ = asFloat(v)
			mod.CompletionRatio, _ = asFloat(completion[name])
			if cr, ok := asFloat(cache[name]); ok {
				mod.CacheRatio, mod.HasCache = cr, true
			}
			// 旧形态没有 enable_groups —— 分组与模型的关系不可知。
			// **不伪造**：此时 GroupModels 为空，调用方据此如实报告"未采到"，
			// 而不是塞进全量模型冒充"该分组能用所有模型"。
			out.Models = append(out.Models, mod)
		}

	default:
		return nil, fmt.Errorf("/api/pricing 的 data 既非数组也非对象（实际 %T）", raw["data"])
	}

	if len(out.Models) == 0 && len(out.GroupRatios) == 0 {
		return nil, fmt.Errorf("/api/pricing 未解析出任何模型或分组（响应结构可能又变了）")
	}

	// 排序让输出稳定：JSON map 遍历顺序随机，不排序会让两次采集的
	// 快照 payload 不同，进而在价格比对时产生假变更。
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].Name < out.Models[j].Name })
	for ref := range out.GroupModels {
		sort.Strings(out.GroupModels[ref])
	}
	return out, nil
}

// toModelPrice 把解析条目转成落库用的 ModelPrice。
//
// 两种计价形态的换算口径**不同**，这里是唯一的转换处：
//   - quota_type=0：值是倍率，原样带出，BillingUnit=per_1m_token，
//     真正的金额由消费方乘站点基准价得出
//   - quota_type=1：model_price 是每次调用的绝对美元价，
//     BillingUnit=per_call，输入/输出同价（按次计费不分输入输出）
//
// completion_ratio 是**相对输入的倍数**而非绝对值 —— 输出价 = 输入价 × 它。
// 漏乘会让输出价被当成倍率本身（实测多数模型该值是 3~4，即输出比输入贵数倍）。
func (m newapiModel) toModelPrice() ModelPrice {
	mp := ModelPrice{ModelName: m.Name, BillingUnit: m.BillingUnit()}
	if m.QuotaType == 1 {
		// 按次计价：输入输出同价，缓存概念不适用
		mp.InputPrice = m.ModelPrice
		mp.OutputPrice = m.ModelPrice
		return mp
	}
	mp.InputPrice = m.ModelRatio
	mp.OutputPrice = m.ModelRatio
	if m.CompletionRatio > 0 {
		mp.OutputPrice = m.ModelRatio * m.CompletionRatio
	}
	if m.HasCache {
		mp.CachePrice = m.ModelRatio * m.CacheRatio
	}
	return mp
}

// jsonUnmarshal 是 encoding/json.Unmarshal 的别名，供测试夹具使用。
var jsonUnmarshal = json.Unmarshal
