package admin

import (
	"strings"
	"testing"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// 这一族测的是一个**纯函数**：输入是已经从库里读出来的事实，输出是一句判定。
// 没有假上游也没有假库 —— 被测对象本身就不碰它们（CLAUDE.md §1 不适用于此）。

func boolp(v bool) *bool { return &v }

func acc(status, credType, credStatus string) store.Account {
	return store.Account{Status: status, CredType: credType, CredStatus: credStatus}
}

var (
	ready   = acc("active", "newapi_access_token", "valid")
	noCred  = acc("active", "", "")
	badCred = acc("active", "newapi_access_token", "needs_relogin")
	off     = acc("disabled", "newapi_access_token", "valid")
)

func TestChannelCollect(t *testing.T) {
	newapi := store.Channel{SiteFamily: "newapi"}
	for _, tc := range []struct {
		name        string
		ch          store.Channel
		noShield    *bool
		accounts    []store.Account
		wantMode    string
		wantBlocker string
		wantReady   int
		wantTotal   int
	}{{
		name: "一切就绪", ch: newapi, noShield: boolp(true),
		accounts: []store.Account{ready, ready},
		wantMode: CollectAuto, wantReady: 2, wantTotal: 2,
	}, {
		name: "部分账号缺凭证", ch: newapi, noShield: boolp(true),
		accounts: []store.Account{ready, noCred},
		wantMode: CollectPartial, wantBlocker: BlockNoCredential, wantReady: 1, wantTotal: 2,
	}, {
		// 站型没有适配器盖过一切：连打哪个端点都不知道。
		name: "站型未识别盖过一切", ch: store.Channel{SiteFamily: "unknown"}, noShield: boolp(true),
		accounts: []store.Account{ready},
		wantMode: CollectManual, wantBlocker: BlockFamilyUnknown,
	}, {
		// 库里的 site_family 可能写着代码里已经删掉的家族（017 收窄过一次）。
		// 那时 CHECK 是过的、注册表是空的，采集侧报"无对应适配器"。
		name: "家族在库里有、注册表里没有", ch: store.Channel{SiteFamily: "asxs"}, noShield: boolp(true),
		accounts: []store.Account{ready},
		wantMode: CollectManual, wantBlocker: BlockFamilyUnknown,
	}, {
		name: "没有账号", ch: newapi, noShield: boolp(true),
		wantMode: CollectManual, wantBlocker: BlockNoAccount,
	}, {
		// "账号全停用"与"一个账号都没有"处置不同：前者是人自己关的。
		name: "账号全被停用", ch: newapi, noShield: boolp(true),
		accounts: []store.Account{off, off},
		wantMode: CollectManual, wantBlocker: BlockAccountDisabled,
	}, {
		name: "全部缺凭证", ch: newapi, noShield: boolp(true),
		accounts: []store.Account{noCred, noCred},
		wantMode: CollectManual, wantBlocker: BlockNoCredential, wantTotal: 2,
	}, {
		name: "凭证在但状态不是 valid", ch: newapi, noShield: boolp(true),
		accounts: []store.Account{badCred},
		wantMode: CollectManual, wantBlocker: BlockNoCredential, wantTotal: 1,
	}, {
		// 停用的账号不进分母：算进去会让一个"两个账号、停了一个、另一个就绪"的
		// 渠道永远停在 partial，而它其实没有任何待办。
		name: "停用账号不计入分母", ch: newapi, noShield: boolp(true),
		accounts: []store.Account{ready, off},
		wantMode: CollectAuto, wantReady: 1, wantTotal: 1,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := channelCollect(tc.ch, tc.noShield, tc.accounts)
			if got.Mode != tc.wantMode {
				t.Errorf("mode = %q，期望 %q（reason: %s）", got.Mode, tc.wantMode, got.Reason)
			}
			if got.Blocker != tc.wantBlocker {
				t.Errorf("blocker = %q，期望 %q", got.Blocker, tc.wantBlocker)
			}
			if got.Ready != tc.wantReady || got.Total != tc.wantTotal {
				t.Errorf("ready/total = %d/%d，期望 %d/%d",
					got.Ready, got.Total, tc.wantReady, tc.wantTotal)
			}
			if strings.TrimSpace(got.Reason) == "" {
				t.Error("reason 不能为空 —— 界面上那个徽标只有它能解释")
			}
		})
	}
}

// **turnstile 不是阻断位。**
//
// 04 §6 写着"开盾站点服务端采集不可行"，而 2026-09-17 实测 api.justwoker.icu
// 的 turnstile_check 为 true、鉴权与 /api/token /api/pricing 全部走通。那一位管的是
// 网页登录表单，我方采集走的是长期访问令牌，根本不经过它。
//
// 这条哨兵盯着：别哪天"顺手把 04 §6 补回来"，那会对着一个采得好好的站告诉运维
// 去人工录数据 —— 比漏报贵，因为人会照着做。
func TestShieldIsANoteNotABlocker(t *testing.T) {
	shielded := channelCollect(store.Channel{SiteFamily: "newapi"}, boolp(false),
		[]store.Account{ready, ready})
	if shielded.Mode != CollectAuto {
		t.Fatalf("站点声明开盾时 mode = %q，期望仍是 auto —— "+
			"turnstile 管的是网页登录，不影响用令牌采集（reason: %s）",
			shielded.Mode, shielded.Reason)
	}
	if shielded.Blocker != "" {
		t.Fatalf("开盾不该产生 blocker，实际 %q", shielded.Blocker)
	}
	// 但要说出来：运维该知道这个站的网页登录需要过人机验证。
	if !strings.Contains(shielded.Reason, "人机验证") {
		t.Fatalf("开盾要在 reason 里提一句，实际：%s", shielded.Reason)
	}
	if shielded.NoShield == nil || *shielded.NoShield {
		t.Fatal("no_shield 要原样回给界面")
	}

	// 从未探测过同理：不改判定，只提一句（FR-020「未采集 ≠ 0」——
	// "没探过"不等于"有盾"，也不等于"无盾"）。
	unprobed := channelCollect(store.Channel{SiteFamily: "newapi"}, nil,
		[]store.Account{ready})
	if unprobed.Mode != CollectAuto || unprobed.Blocker != "" {
		t.Fatalf("没探测过不该改判定，实际 mode=%q blocker=%q", unprobed.Mode, unprobed.Blocker)
	}
	if !strings.Contains(unprobed.Reason, "还没探测过") {
		t.Fatalf("没探测过要提一句，实际：%s", unprobed.Reason)
	}
	if unprobed.NoShield != nil {
		t.Fatal("没探测过时 no_shield 必须缺席，不能填一个 false 冒充「有盾」")
	}
}

func TestAccountCollect(t *testing.T) {
	newapi := store.Channel{SiteFamily: "newapi"}
	for _, tc := range []struct {
		name        string
		ch          store.Channel
		noShield    *bool
		a           store.Account
		wantMode    string
		wantBlocker string
	}{{
		name: "就绪", ch: newapi, noShield: boolp(true), a: ready, wantMode: CollectAuto,
	}, {
		// 这一条是账号级判定存在的理由之一：只看 cred_type 的话它是"已登记"，
		// 而所属渠道站型没有适配器时它一把都采不到。
		name: "渠道站型未识别时凭证齐备也采不了",
		ch:   store.Channel{SiteFamily: "unknown"}, noShield: boolp(true), a: ready,
		wantMode: CollectManual, wantBlocker: BlockFamilyUnknown,
	}, {
		name: "没凭证", ch: newapi, noShield: boolp(true), a: noCred,
		wantMode: CollectManual, wantBlocker: BlockNoCredential,
	}, {
		name: "凭证需重登", ch: newapi, noShield: boolp(true), a: badCred,
		wantMode: CollectManual, wantBlocker: BlockCredentialInvalid,
	}, {
		name: "账号已停用", ch: newapi, noShield: boolp(true), a: off,
		wantMode: CollectManual, wantBlocker: BlockAccountDisabled,
	}, {
		name: "渠道没探测过也照常判定", ch: newapi, noShield: nil, a: ready,
		wantMode: CollectAuto,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := accountCollect(tc.ch, tc.noShield, tc.a)
			if got.Mode != tc.wantMode {
				t.Errorf("mode = %q，期望 %q（reason: %s）", got.Mode, tc.wantMode, got.Reason)
			}
			if got.Blocker != tc.wantBlocker {
				t.Errorf("blocker = %q，期望 %q", got.Blocker, tc.wantBlocker)
			}
			if strings.TrimSpace(got.Reason) == "" {
				t.Error("reason 不能为空")
			}
		})
	}
}

// 凭证状态非 valid 时**不能**算进 ready：CredentialStore.ListByChannel 的
// WHERE 里就写着 status='valid' AND a.status='active'，采集侧压根不会拿到这条凭证。
// 两边的判据必须是同一条，否则界面说"可自动"、采集器说"没有可用凭证"。
func TestReadyMatchesWhatCollectorWouldUse(t *testing.T) {
	got := channelCollect(store.Channel{SiteFamily: "newapi"}, boolp(true),
		[]store.Account{badCred, off, ready})
	if got.Ready != 1 {
		t.Fatalf("ready = %d，期望 1 —— 只有 status=valid 且账号 active 的那条会被采集侧取到", got.Ready)
	}
}
