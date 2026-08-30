package collector

import (
	"strings"
	"time"
)

// ASXS（闭源自建平台）的注册。
//
// **全自研站怎么接：一份注册 + 一个实现 Adapter 的文件。** ASXS 就是这条
// 路径的活样本 —— 它与 NewAPI/Sub2API 零复用（04 总原则：一站一适配器、
// 不作探测基准），而接它需要动的地方全在这份注册里。
var regASXS = &Registration{
	Family:      FamilyASXS,
	DisplayName: "ASXS（闭源）",

	ProbePath: "/api/public/site-config",
	// 命中特征：200 且 JWT iss 为 ampmanager（04 §2/§3.3）。
	// 站点可能不在 JSON 里直接写 iss，故也扫原文 —— 这是闭源平台的
	// 唯一稳定指纹（其命名空间与 NewAPI/Sub2API 零重叠）。
	Match: func(m map[string]any, raw []byte) bool {
		if strings.Contains(string(raw), "ampmanager") {
			return true
		}
		d := unwrapData(m)
		return str(d["iss"]) == "ampmanager"
	},
	Extract: func(m map[string]any, r *DetectResult) {
		r.Version = str(unwrapData(m)["version"])
		// ASXS 无 turnstile 概念；不声称无盾，留 false 由运维确认
	},

	Aliases: []string{"asxs", "ampmanager"},

	CredType: "asxs_jwt",
	// 无 refresh 路径，故也接受账密 —— 那是它唯一的续期方式。
	PasswdCredType: "account_password",
	CredNote:       "它无 refresh 路径，续期只能账密重登（04 §5.3）",

	// JWT 有效期 7 天且**无 refresh 路径**，只能账密重登（04 §5.3）。
	// 剩余不足 1 天即重登：频率约每周一次，风控压力小。
	RefreshLead: 24 * time.Hour,

	New: func(c *Client) Adapter { return NewASXSAdapter(c) },
}
