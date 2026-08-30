package collector

import (
	"fmt"
	"strings"
	"time"
)

// 站型注册表：**加一个站型 = 加一份 Registration + 一个 Adapter**。
//
// 为什么要有它：此前一个站型的信息散在九处 —— Family 常量、detectSteps、
// adapterFor、authHeaders、NeedsRefresh、凭证必需字段校验、两处 credType
// 映射、导入侧别名表。前几处漏了会编译错或探测不到（看得见），而
// **authHeaders 与 NeedsRefresh 漏了是静默的**：编译过、单测过，
// 跑起来才 401 或令牌悄悄过期不续。
//
// 收表之后那两处不再逐家族分流（见 httpx.go 的 authHeaders 与 auth.go 的
// NeedsRefresh），"漏一处"的检出交给 registry_test.go 的四条静态断言。

// Registration 是一个站型的全部可变点。
//
// 判据：**只有逐家族真的不同的东西才进这里。** 三家族一致的行为不是变化点
// 而是常量 —— 给它开一个字段只会让下一个站型以为自己必须填。
// 鉴权头就是这么被删掉的：三家族都是 `Bearer <token>` + 有则带用户 ID 头，
// 原先那个 switch 表达的是"零个变化点"。
type Registration struct {
	Family Family
	// DisplayName 是给人看的名字：界面的站型下拉、凭证校验的错误信息都用它。
	DisplayName string

	// ── 探测（04 §2）──
	// ProbePath 是判族用的公开端点；Match 判定响应是否属本族；
	// Extract 从命中的响应里取家族特有信息（版本、无盾、额度换算基数）。
	//
	// Match 同时收到**解析后的 map 与原始字节**。两个参数不是冗余：
	// 现役两族的指纹都在顶层 JSON 字段里，于是都写 `_ []byte`；
	// 而自研站的指纹可能压根不在 JSON 里（实测过一例：指纹在 JWT 的 iss
	// 声明里，响应体本身还不一定是合法 JSON）。留着 raw 的代价是两个下划线，
	// 换来的是"接一个自研站只加一份 Registration"而不必改这个签名、
	// 改 detect.go、再回头改另外两族 —— 见 04 §7bis。
	// detect.go 的 getJSON 在 JSON 解析失败时仍回传 raw，
	// TestDetectPassesRawBodyToMatch 钉住这条通路。
	ProbePath string
	Match     func(m map[string]any, raw []byte) bool
	Extract   func(m map[string]any, r *DetectResult)

	// Aliases 是这个站型在 all-api-hub 导出的 site_type 里的各种自称。
	//
	// ⚠️ **只准填实测见过的值**（CLAUDE.md §1）：这张表用于比对"导出声明与
	// 我方探测是否一致"，凭想象加别名会让本该报出来的声明错变成静默采信。
	Aliases []string

	// ── 凭证形态（09 §5 的凭证登记与导入侧登记共用）──
	// CredType 是有 access_token 时的凭证类型；RequiresUID 表示还必须有
	// external_user_id；PasswdCredType 非空表示允许账密。
	// CredNote 是校验失败时附在错误后的"为什么"，省得运维回查文档。
	//
	// PasswdCredType **当前无人声明**（现役两族都有令牌路径），留着是因为它是
	// 自研站最可能落在的那一格：自建面板常只有登录表单、没有令牌端点，
	// 唯一续期方式就是账密重登。这一格连着 Credential.Username/Password、
	// CredTypeFor 的 hasPasswd 分支、库侧 cred_type 的 account_password
	// 取值 —— 四处一起留才是一条通路，删掉任一处都要在接入时重新拉一遍。
	// 接入清单见 04 §7bis。
	CredType       string
	RequiresUID    bool
	PasswdCredType string
	CredNote       string

	// RefreshLead 是"到期前多久主动续期"。
	// **0 = 永不主动续期**，且据此推导该站型不需要 Refresher。
	RefreshLead time.Duration

	// TokenExpiryFrom 从 access_token 本身读出到期时间，读不出返回 false。
	//
	// 为什么必须有它：**登记路径拿不到到期时间。** saveCredential 与导入侧
	// 构造 Credential 时都不带 TokenExpiresAt —— all-api-hub 导出里只有
	// access_token（实测：`account_info` 的键只有 access_token/id/quota/
	// username/today_* 那几个，没有到期时间也没有 refresh_token）。于是库里
	// token_expires_at 全是 NULL，而 NeedsRefresh 在零值时返回 false ——
	// RefreshLead 对登记进来的凭证就此形同虚设，令牌到期后只能等某次采集撞
	// 401，且 401 之后 sync.go 直接中止、没有反应式补救。主动续期变成"只在
	// 已经续过一次之后才生效"的自锁。
	//
	// 2026-08-30 实测确认这条路径不可达：65 渠道全量 sync，续期动作 0 次，
	// 13 个 sub2api 全 401；而那些 JWT 的 exp 声明分别是 2026-03（8 条）与
	// 2026-08-27~29（3 条）—— **到期时间一直写在令牌里，只是没人读**。
	//
	// nil = 该站型的令牌里读不出到期时间（NewAPI 的长期令牌就是这样，
	// 它同时 RefreshLead=0，两处一致）。registry_test.go 双向钉住
	// "RefreshLead>0 ⟺ TokenExpiryFrom 非 nil"：单边会让这一格重新变成死的。
	TokenExpiryFrom func(accessToken string) (time.Time, bool)

	// New 构造该站型的适配器。**是否支持续期不在这里声明** ——
	// 由 Adapter 有没有实现 Refresher 决定（cmd/sla-core/sync.go 的类型断言），
	// 声明与实现因此不可能不一致。
	New func(*Client) Adapter
}

// registrations 的顺序**就是探测顺序**（04 §2）：判据越通用的放越前面，
// 越特殊的放后面。NewAPI 的 /api/status 最通用（二开站也大多留着它），
// 放第一位能最快分桶。**新加的自研站放最后** —— 它的判据只认自己那一站，
// 放前面只是让另外两族每次多一跳无谓的探测。
//
// 用一个显式切片而不是各文件 init() 自注册：init() 的执行顺序取决于文件名，
// 那会让探测顺序被一次无关的重命名悄悄改掉。
var registrations = []*Registration{
	regNewAPI,
	regSub2API,
}

var byFamily = func() map[Family]*Registration {
	m := make(map[Family]*Registration, len(registrations))
	for _, r := range registrations {
		m[r.Family] = r
	}
	return m
}()

// Lookup 取某站型的注册。
//
// unknown 与未注册的站型一律返回 false，**不回退到某个家族**：
// 猜错会让全部字段映射错位，而错误的余额/额度比没有数据更危险（04 §7）。
func Lookup(f Family) (*Registration, bool) {
	r, ok := byFamily[f]
	return r, ok
}

// All 按探测顺序返回全部注册。
func All() []*Registration { return registrations }

// FamilyOfAlias 把导出数据里的 site_type 自称映射到家族，认不出返回 unknown。
func FamilyOfAlias(declared string) Family {
	s := strings.ToLower(strings.TrimSpace(declared))
	for _, r := range registrations {
		for _, a := range r.Aliases {
			if a == s {
				return r.Family
			}
		}
	}
	return FamilyUnknown
}

// CredTypeFor 校验登记进来的凭证字段够不够用，并给出凭证类型。
//
// 校验与选型**必须一处做完**：分开写时它们各有一个 switch，而漏改选型那处
// 的后果是 cred_type 空串进库 —— 校验是绿的，采集时才炸。
func (r *Registration) CredTypeFor(hasToken, hasUID, hasPasswd bool) (string, error) {
	if hasToken {
		if r.RequiresUID && !hasUID {
			return "", fmt.Errorf("%s 需要 external_user_id（用户 ID 头的值）—— %s",
				r.DisplayName, r.CredNote)
		}
		return r.CredType, nil
	}
	if r.PasswdCredType != "" && hasPasswd {
		return r.PasswdCredType, nil
	}
	want := "access_token"
	if r.RequiresUID {
		want += " 与 external_user_id"
	}
	if r.PasswdCredType != "" {
		want += "，或 username + password"
	}
	return "", fmt.Errorf("%s 需要 %s —— %s", r.DisplayName, want, r.CredNote)
}
