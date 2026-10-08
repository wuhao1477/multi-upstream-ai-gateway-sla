package admin

import (
	"fmt"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// 「这个渠道 / 账号能不能全自动采集」的判定。
//
// 为什么要有这一处：判据原先散在三个互不相干的地方 —— 站型在渠道行、账号数要数、
// 凭证在账号行 —— 谁都没把它们合成运维真正要问的那句话：**哪些站我不用管，
// 哪些站得我自己去录数据。** 于是一个站型未识别、永远采不到任何东西的渠道，
// 在列表上看起来跟正常的一模一样。
//
// 判定放服务端而不是前端拼：这三条跟采集器自己用的**是同一套判据** ——
// 站型要在注册表里有（Lookup）、凭证要 status='valid'、账号要 active
//（CredentialStore.ListByChannel 的 WHERE 逐字如此）。写在前端就会有两份，
// 而漂移的方向一定是界面说"能采"、采集器说"采不了"。
//
// ⚠️ 这里回答的是**能不能**，不是**采没采到**。渠道的「启用状态」是另一根轴
//（人有没有把它关掉），两者原先都叫「状态」，于是一个 enabled 但根本采不了的
// 渠道看起来完全正常（ChannelsView 顶部注释的同一条教训）。
//
// ── 为什么 turnstile 不在判据里 ────────────────────────────────────────────
//
// [04 §6] 写着「接入前必须确认 turnstile_check:false，**开盾站点服务端采集
// 不可行**，须转人工录入」。这一版**没有**照着它把开盾站点判成"需人工"，理由是
// 实测站不住：
//
//	2026-09-17 实测 api.justwoker.icu（JustDoWork）：/api/status 的
//	turnstile_check 为 true，而鉴权、/api/token、/api/pricing **全部走通**，
//	Key 与定价都采得到。
//
// 那一位管的是**网页登录表单**，而我方采集根本不走登录 —— 用的是人早就拿到手的
// 长期访问令牌。把它当阻断位，等于对着一个采得好好的站告诉运维"去人工录数据"，
// 而那是比漏报更贵的错：人会照着做。
//
// 真正过不去的那种盾（anyrouter.top 那类整站 JS 盾）**不走这条路** ——
// 它连 /api/status 都返回混淆 JS，Detect 直接认不出家族，落到 family_unknown。
// 所以"真的采不了"这一档已经被盖住了，且是被**证据**盖住的，不是被一个声明位。
//
// no_shield 仍然回给界面，但只作为一句提示（见 shieldNote），不改 mode。
// 单个反例不足以改 04 §6 的接入纪律（那是"接入前要看一眼"），只足以说明
// **不能拿它当自动判据**；再攒几个样本可以回去重审那一条。

// 采集模式。三档，每一档都由证据撑着：
const (
	// CollectAuto：全部启用账号都能自动采，不需要人做任何事。
	CollectAuto = "auto"
	// CollectPartial：一部分账号能采，另一部分不能 —— 只在渠道级出现。
	CollectPartial = "partial"
	// CollectManual：现在一个都采不了，要人动手。**为什么**看 Blocker/Reason。
	CollectManual = "manual"
)

// Blocker 的取值。给验收与界面配色用的机器可读判据，别拿 Reason 去做判断 ——
// 那是给人读的，会改。
const (
	// BlockFamilyUnknown：站型未识别或没有对应注册，没有适配器可用（04 §7）。
	// **这一档不是配置问题**：探测都没认出这是什么站，登记多少凭证都没用。
	// 整站 JS 盾的站点也落在这里 —— 它连 /api/status 都不回 JSON。
	BlockFamilyUnknown = "family_unknown"
	// BlockNoAccount：还没登记账号。没有账号就没有凭证，也没有采集对象。
	BlockNoAccount = "no_account"
	// BlockNoCredential：账号在，但没登记可用的采集凭证。
	BlockNoCredential = "no_credential"
	// BlockCredentialInvalid：凭证登记了但状态不是 valid（04 §5 状态机）。
	BlockCredentialInvalid = "credential_invalid"
	// BlockAccountDisabled：账号被人工停用，采集会跳过它。
	BlockAccountDisabled = "account_disabled"
)

// Collect 是一条「能不能自动采」的判定，挂在渠道行与账号行上。
type Collect struct {
	Mode string `json:"mode"`
	// Blocker 为空 = 没有阻碍。非空时是上面那组常量之一。
	Blocker string `json:"blocker,omitempty"`
	// Reason 给人读，会改；要判断请用 Mode/Blocker。
	Reason string `json:"reason"`
	// Ready/Total 只在渠道级有意义：能自动采的账号数 / 启用中的账号数。
	Ready int `json:"ready_accounts,omitempty"`
	Total int `json:"total_accounts,omitempty"`
	// NoShield 是站点 /api/status 自称的「人机验证已关」。
	// **缺席 = 从未探测过**，不是"有盾"（FR-020「未采集 ≠ 0」的同一条纪律）。
	// 它不参与 Mode 的判定，只在 Reason 后面附一句 —— 理由见文件头。
	NoShield *bool `json:"no_shield,omitempty"`
}

// shieldNote 是跟在 Reason 后面的那句提示。
//
// 两种情形各说各的，且都**不改 mode**：站点声明开着人机验证时说清"那一位管的是
// 网页登录"，免得运维看见它就去人工录；从未探测过时说一句，因为 04 §6 要求接入前
// 看一眼，而"没看过"本身是条信息。
func shieldNote(noShield *bool) string {
	switch {
	case noShield == nil:
		return "；站点还没探测过（04 §6 建议接入前确认一次）"
	case !*noShield:
		return "；站点声明开着人机验证（turnstile）—— 实测那一位管的是网页登录，" +
			"不影响用访问令牌采集，故不据此判为需人工"
	default:
		return ""
	}
}

// familyBlocked 判站型这一关。查注册表而不是只比 'unknown'：库里的 site_family
// 可能写着一个**代码里已经不存在**的家族（017 收窄过一次取值），那时 CHECK 是过的、
// 注册表是空的，采集侧报"无对应适配器"—— 而界面会显示一个煞有介事的站型名。
func familyBlocked(family string) string {
	if family == "" || family == string(collector.FamilyUnknown) {
		return "站型未识别，没有对应适配器；先重新探测，或按 04 §7 接一个专属适配器" +
			"（整站 JS 盾的站点也落在这里：它连 /api/status 都不回 JSON）"
	}
	if _, ok := collector.Lookup(collector.Family(family)); !ok {
		return "站型 " + family + " 在注册表里没有对应适配器，采集必然失败（04 §7）"
	}
	return ""
}

// channelCollect 汇总一个渠道的采集能力。
//
// accounts 只传该渠道的账号；noShield 为 nil 表示从未探测过。
func channelCollect(ch store.Channel, noShield *bool, accounts []store.Account) Collect {
	c := Collect{NoShield: noShield}

	if reason := familyBlocked(ch.SiteFamily); reason != "" {
		c.Mode, c.Blocker, c.Reason = CollectManual, BlockFamilyUnknown, reason
		return c
	}

	for _, a := range accounts {
		// 停用的账号不进分母：算进去会让一个"两个账号、停了一个、另一个就绪"的
		// 渠道永远停在 partial，而它其实没有任何待办。
		if a.Status != "active" {
			continue
		}
		c.Total++
		if accountCollect(ch, nil, a).Mode == CollectAuto {
			c.Ready++
		}
	}

	switch {
	case c.Total == 0:
		c.Mode, c.Blocker = CollectManual, BlockNoAccount
		// 区分"一个账号都没有"与"账号全被停用了"：后者是人自己关的，
		// 提示他去登记新账号只会让人困惑。
		if len(accounts) == 0 {
			c.Reason = "还没有登记账号 —— 没有账号就没有凭证，也没有采集对象"
		} else {
			c.Blocker = BlockAccountDisabled
			c.Reason = fmt.Sprintf("%d 个账号全部已停用，采集会跳过这个渠道", len(accounts))
		}
	case c.Ready == 0:
		c.Mode, c.Blocker = CollectManual, BlockNoCredential
		c.Reason = fmt.Sprintf("%d 个启用账号都没有可用的采集凭证，登记后即可自动采集（04 §5）", c.Total)
	case c.Ready < c.Total:
		c.Mode, c.Blocker = CollectPartial, BlockNoCredential
		c.Reason = fmt.Sprintf("%d/%d 个启用账号可自动采集，其余缺凭证或凭证失效", c.Ready, c.Total)
	default:
		c.Mode = CollectAuto
		c.Reason = fmt.Sprintf("%d 个启用账号全部可自动采集", c.Total)
	}
	c.Reason += shieldNote(noShield)
	return c
}

// accountCollect 判单个账号。渠道层面的阻碍照原样传下来 ——
// 站型没有适配器时，这个账号凭证再全也采不了，说它"可自动"是骗人。
func accountCollect(ch store.Channel, noShield *bool, a store.Account) Collect {
	c := Collect{NoShield: noShield}

	if reason := familyBlocked(ch.SiteFamily); reason != "" {
		c.Mode, c.Blocker, c.Reason = CollectManual, BlockFamilyUnknown, reason
		return c
	}
	switch {
	case a.Status != "active":
		c.Mode, c.Blocker = CollectManual, BlockAccountDisabled
		c.Reason = "账号已停用，采集会跳过它"
	case a.CredType != "" && a.CredStatus == "valid":
		c.Mode, c.Reason = CollectAuto, "站型已识别、令牌有效 —— 可全自动采集"
	case ch.SiteFamily == "newapi" && a.CookieConfigured && a.CookieEnabled:
		c.Mode, c.Blocker = CollectManual, BlockCredentialInvalid
		switch a.CookieState {
		case "ready":
			c.Mode, c.Blocker, c.Reason = CollectAuto, "", "Cookie 已验证，可自动读取"
		case "expired":
			c.Reason = "Cookie 已失效，请在原站登录后重新导入 Cookie"
		case "needs_action":
			c.Reason = "Cookie 需要人工处理，请检查原站会话和登记的上游用户 ID"
		default:
			c.Reason = "Cookie 尚未验证，请点击验证 Cookie"
		}
	case a.CredType == "":
		c.Mode, c.Blocker = CollectManual, BlockNoCredential
		c.Reason = "没有登记采集凭证，登记后即可自动采集（04 §5 按站型给不同字段）"
	case a.CredStatus != "valid":
		c.Mode, c.Blocker = CollectManual, BlockCredentialInvalid
		c.Reason = "采集凭证状态是 " + a.CredStatus + "，需要重新登记（04 §5）"
	default:
		c.Mode = CollectAuto
		c.Reason = "站型已识别、凭证有效 —— 可全自动采集"
	}
	c.Reason += shieldNote(noShield)
	return c
}

// shieldOf 把 DetectedNoShield 的 map 查成三态指针。
func shieldOf(shields map[int64]bool, channelID int64) *bool {
	v, ok := shields[channelID]
	if !ok {
		return nil
	}
	return &v
}
