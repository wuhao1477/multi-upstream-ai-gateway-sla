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
	CredNote: "access_token 是 JWT（实测 24h 有效），续期走 /api/v1/auth/refresh（04 §5.2）",

	// 到期前 120s 主动刷新 —— Sub2API 官方前端用的就是这个值
	// （SUB2API_TOKEN_REFRESH_BUFFER_MS，04 §5.2）。太短会在刷新失败时
	// 没有重试余量。刷新会轮换 refresh_token，故必须走 Authenticator
	// 的账号锁（不变式 S-1）。
	RefreshLead: 120 * time.Second,

	New: func(c *Client) Adapter { return NewSub2APIAdapter(c) },
}
