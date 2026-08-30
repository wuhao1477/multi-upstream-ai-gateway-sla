package collector

import "time"

// Sub2API 系的注册。
var regSub2API = &Registration{
	Family:      FamilySub2API,
	DisplayName: "Sub2API 系",

	ProbePath: "/api/v1/settings/public",
	// 命中特征：含 site_name / turnstile_enabled，且是 {code,message,data} envelope
	Match: func(m map[string]any, _ []byte) bool {
		d := unwrapData(m)
		_, hasSite := d["site_name"]
		_, hasTurnstile := d["turnstile_enabled"]
		return hasSite || hasTurnstile
	},
	Extract: func(m map[string]any, r *DetectResult) {
		d := unwrapData(m)
		r.Version = str(d["version"])
		r.NoShield = !boolOf(d["turnstile_enabled"])
	},

	Aliases: []string{"sub2api"},

	CredType: "sub2api_jwt",
	CredNote: "access_token 是 JWT（带 exp 声明），续期走 /api/v1/auth/refresh（04 §5.2）",

	// 到期前 120s 主动刷新 —— Sub2API 官方前端用的就是这个值
	// （SUB2API_TOKEN_REFRESH_BUFFER_MS，04 §5.2）。太短会在刷新失败时
	// 没有重试余量。刷新会轮换 refresh_token，故必须走 Authenticator
	// 的账号锁（不变式 S-1）。
	RefreshLead: 120 * time.Second,

	// 到期时间从 JWT 自己的 exp 读，**不按"24h"推算**。
	//
	// 实测（2026-08-30，真库 13 条凭证）：exp 分别落在 2026-03（8 条）与
	// 2026-08-27~29（3 条），另有 2 条内容不是三段 JWT。也就是说这些令牌的
	// 实际有效期跨度远不止 24h —— 04 §5.2 记的 24h 是官方前端的刷新缓冲，
	// 不是令牌自己声明的到期时间。按 24h 从"登记时刻"推算会得出一个与真实
	// exp 无关的时间，那比不知道更糟。
	TokenExpiryFrom: jwtExpiry,

	New: func(c *Client) Adapter { return NewSub2APIAdapter(c) },
}
