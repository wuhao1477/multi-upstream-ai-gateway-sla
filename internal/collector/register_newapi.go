package collector

// NewAPI 系（含二开）的注册。
//
// **魔改站怎么接：同一个 New，只加 Aliases。** 实测二开站改的是两处 ——
// 用户 ID 头名（Veloera-User / Rix-Api-User…）由 Authenticate 的 fan-out
// 逐个试探（auth.go 的 newAPIUserIDHeaders），分页信封（data.items vs
// data.records）由 unwrapDataList 两个分支都吃下。**都不需要新适配器**，
// 所以这里既没有"头名列表"字段也没有"信封类型"字段。
var regNewAPI = &Registration{
	Family:      FamilyNewAPI,
	DisplayName: "NewAPI 系",

	ProbePath: "/api/status",
	// 命中特征：含 quota_per_unit / turnstile_check / checkin_enabled。
	// 用"任一存在"而非"全部存在"：二开站点会删字段，但不会全删。
	Match: func(m map[string]any, _ []byte) bool {
		d := unwrapData(m)
		for _, k := range []string{"quota_per_unit", "turnstile_check", "checkin_enabled"} {
			if _, ok := d[k]; ok {
				return true
			}
		}
		return false
	},
	Extract: func(m map[string]any, r *DetectResult) {
		d := unwrapData(m)
		r.Version = str(d["version"])
		// turnstile_check=true 表示开盾 → 服务端采集不可行（04 §6）
		r.NoShield = !boolOf(d["turnstile_check"])
		// ⚠️ 逐站读取，**不写死**：upstream-d.invalid 是 500000，别家不一定（04 §2）
		r.QuotaPerUnit = floatOf(d["quota_per_unit"])
	},

	// 实测在 all-api-hub 导出里见过的自称。**不凭想象加**（CLAUDE.md §1）。
	Aliases: []string{"new-api", "newapi", "rix-api"},

	CredType:    "newapi_access_token",
	RequiresUID: true,
	CredNote:    "只带 Authorization 必然 401（04 §3.1）",

	// 0 = 永不主动续期。**不变式 N-1**：/api/user/token 是"重新生成"而非
	// "读取"，调它会立刻作废正在使用的令牌。只有 401 才说明令牌被后台重置，
	// 那时需人工重登（04 §5.1）。NewAPIAdapter 因此没有 Refresh 方法 ——
	// 那个"没有"就是本条的实现侧声明，registry_test 双向钉住两者一致。
	RefreshLead: 0,

	New: func(c *Client) Adapter { return NewNewAPIAdapter(c) },
}
