package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// 凭证生命周期的两条硬约束（04 §5，"采集器最容易出事的地方"）。
//
// 不变式 N-1（NewAPI）：**运行时禁止调用 /api/user/token**。
//
//	实测该端点是"重新生成"而非"读取" —— 每次调用都返回新令牌并**立即作废旧
//	令牌**，会踢掉正在使用的令牌。故只在初始化时取一次并持久化。
//	代码层面的保障：newapi 适配器不提供任何调用该端点的方法，
//	且下方 ErrTokenRegenForbidden 用于在误用时显式失败。
//
// 不变式 S-1（Sub2API）：**同账号刷新必须串行**。
//
//	refresh 会轮换 refresh_token，并发刷新会互相作废（A 拿到新 token 的同时
//	B 手上的 refresh_token 已失效）。故按账号加互斥锁，且**刷新结果先持久化
//	再释放锁** —— 先释放锁会让下一个等待者读到旧值又去刷一次。
var (
	// ErrTokenRegenForbidden：试图在运行时重新生成 NewAPI 系统访问令牌。
	ErrTokenRegenForbidden = errors.New(
		"collector: 运行时禁止重新生成 NewAPI 访问令牌（不变式 N-1：会作废正在使用的令牌）")
	// ErrNeedsRelogin：凭证已无法自动续期，需人工重新登录（04 §5 第 4 层）。
	ErrNeedsRelogin = errors.New("collector: 需要重新登录")
)

// 续期阈值不在这里 —— 它逐家族不同，值与理由都写在各自的
// Registration.RefreshLead（register_*.go）。原先这里有两个阈值常量，
// 而 NeedsRefresh 里有一个 switch 把它们对上家族 —— 那个 switch 就是
// "加站型要改九处"里静默失败的那处。

// CredentialStore 是凭证持久化契约。
//
// 抽成接口而非直接依赖 store 包：collector 不该反向依赖存储层的具体实现，
// 且这样才能对"刷新结果先持久化再释放锁"做单测（04 §5.2 不变式 S-1）。
type CredentialStore interface {
	// WithRefreshLock serializes, reloads, and persists one credential refresh.
	WithRefreshLock(ctx context.Context, cred Credential,
		refresh func(Credential) (Credential, bool, error)) (Credential, error)
}

// Authenticator 按站型执行鉴权与续期，并保证两条硬约束。
type Authenticator struct {
	Store CredentialStore
}

// NewAuthenticator 构造鉴权器。
func NewAuthenticator(store CredentialStore) *Authenticator {
	return &Authenticator{Store: store}
}

// RefreshLockKey returns the stable account-level lock key persisted with a credential.
func RefreshLockKey(cred Credential) string {
	if key := strings.TrimSpace(cred.RefreshLockKey); key != "" {
		return key
	}
	if cred.Family != FamilySub2API {
		return ""
	}
	accountID := strings.TrimSpace(cred.ExternalUserID)
	if accountID == "" {
		var claims struct {
			UserID json.RawMessage `json:"user_id"`
		}
		if decodeJWTPayload(cred.AccessToken, &claims) {
			if len(claims.UserID) > 0 && claims.UserID[0] == '"' {
				_ = json.Unmarshal(claims.UserID, &accountID)
			} else {
				accountID = string(claims.UserID)
			}
			accountID = strings.TrimSpace(accountID)
		}
	}
	if accountID == "" || accountID == "null" {
		accountID = fmt.Sprintf("channel:%d", cred.ChannelID)
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cred.BaseURL), "/")
	if u, err := url.Parse(baseURL); err == nil {
		u.Scheme = strings.ToLower(u.Scheme)
		u.Host = strings.ToLower(u.Host)
		baseURL = u.String()
	}
	return fmt.Sprintf("refresh:%s:%s:%s", cred.Family, baseURL, accountID)
}

// Refresher 是站型特定的续期实现。
//
// **实现它就等于声明"本族要主动续期"**（collection.Runner 靠类型断言取，
// registry_test 双向钉住它与 RefreshLead 一致），所以不要"顺手实现一个"。
// 已知形态：令牌换令牌（Sub2API：POST /api/v1/auth/refresh，会轮换
// refresh_token，故必须走账号锁）。
// NewAPI 刻意不实现：长期令牌，不变式 N-1 禁止重新生成。
type Refresher interface {
	// Refresh 用当前凭证换取新凭证。
	// 已无法自动续期时返回 ErrNeedsRelogin（04 §5 第 4 层），
	// **不要返回一个空凭证** —— 那会让采集侧带着空令牌去打上游。
	Refresh(ctx context.Context, cred Credential) (Credential, error)
}

// NeedsRefresh 判断凭证是否需要续期。
//
// 阈值逐家族不同，值与理由都在各自的 Registration.RefreshLead
// （现役：NewAPI 0 = 永不主动刷新 / Sub2API 120s，04 §5）。
//
// 这一处曾是"加站型要改九处"里最坏的两处之一：漏加 case 落到 return false，
// 后果是**令牌到期不续**，而它不报错、不 401（还没到期时一切正常），
// 只在某次采集突然全挂时才被发现。改读注册表后，漏注册整个站型会被
// registry_test 的第一条断言拦下，而注册了就必然有 RefreshLead。
func NeedsRefresh(cred Credential, now time.Time) bool {
	reg, ok := Lookup(cred.Family)
	if !ok || reg.RefreshLead == 0 {
		// 0 = 永不主动续期（NewAPI 的不变式 N-1：主动刷新会把自己踢下线，
		// 只有 401 才说明令牌被后台重置，那时需人工介入）。
		return false
	}
	exp := cred.TokenExpiresAt
	if exp.IsZero() && reg.TokenExpiryFrom != nil {
		// **登记路径不带到期时间**（saveCredential 与导入侧都不填，导出里也
		// 没有），所以零值不等于"没有到期时间"，只等于"库里没记"。到期时间
		// 常常就写在令牌自己里（Sub2API 的 JWT 有 exp 声明），读它。
		//
		// 不读的后果实测过：65 渠道全量 sync 续期动作 0 次、13 个 sub2api 全
		// 401，而它们的 exp 早已过期 —— RefreshLead 只在"已经续过一次之后"
		// 才生效，那是个自锁。见 Registration.TokenExpiryFrom。
		if t, ok := reg.TokenExpiryFrom(cred.AccessToken); ok {
			exp = t
		}
	}
	if exp.IsZero() {
		// 到这里才是真的无从判断：库里没记，令牌里也读不出
		//（NewAPI 的长期令牌本就没有到期时间；真库里还有两条 cred_type 写
		// sub2api_jwt 而内容不是三段 JWT 的凭证）。**不猜默认值** ——
		// 猜一个会让它按别人的节奏刷自己的令牌。
		// （P1-evidence §4 第 5 项：用 time.Time 扫 NULL 让整族采不成）
		return false
	}
	return now.Add(reg.RefreshLead).After(exp)
}

// EnsureFresh 在需要时续期凭证，返回可用的凭证。
//
// **不变式 S-1 的落地**：整个"判定 → 刷新 → 持久化"在账号锁内完成。
// 锁内二次判定（double-check）是必要的：等锁期间前一个持有者可能已经刷过了，
// 此时再刷一次会作废刚拿到的 refresh_token —— 那正是并发刷新互相作废的场景。
func (a *Authenticator) EnsureFresh(
	ctx context.Context, cred Credential, r Refresher, now time.Time,
) (Credential, error) {
	if !NeedsRefresh(cred, now) {
		return cred, nil
	}
	if a.Store == nil {
		return cred, fmt.Errorf("续期失败（%s/渠道 %d）：凭证存储未配置",
			cred.Family, cred.ChannelID)
	}
	fresh, err := a.Store.WithRefreshLock(ctx, cred,
		func(cur Credential) (Credential, bool, error) {
			if !NeedsRefresh(cur, time.Now()) {
				return cur, false, nil
			}
			fresh, err := r.Refresh(ctx, cur)
			if err != nil {
				return cur, false, fmt.Errorf("续期失败（%s/渠道 %d）: %w",
					cred.Family, cred.ChannelID, err)
			}
			return fresh, true, nil
		})
	if err != nil {
		return cred, err
	}
	return fresh, nil
}

// SessionFrom 把凭证转成会话句柄。
func SessionFrom(cred Credential, baseURL string, quotaPerUnit float64) Session {
	return Session{
		ChannelID:      cred.ChannelID,
		Family:         cred.Family,
		BaseURL:        baseURL,
		Token:          cred.AccessToken,
		UserIDHeader:   cred.UserIDHeaderName,
		ExternalUserID: cred.ExternalUserID,
		ExpiresAt:      cred.TokenExpiresAt,
		QuotaPerUnit:   quotaPerUnit,
	}
}

// newAPIUserIDHeaders 是 NewAPI 二开的用户 ID 头名候选（04 §3.1）。
//
// 首次鉴权时逐一试探，命中后记入凭证；顺序按实测常见度排列。
// **必须 fan-out**：只带 Cookie 会 401，而二开站点改了头名（04 §3.1）。
var newAPIUserIDHeaders = []string{
	"New-API-User",
	"Veloera-User",
	"X-Api-User",
	"voapi-user",
	"User-id",
	"Rix-Api-User",
	"neo-api-user",
}

// NewAPIUserIDHeaderCandidates 返回待试探的头名列表（供适配器与测试使用）。
func NewAPIUserIDHeaderCandidates() []string {
	out := make([]string, len(newAPIUserIDHeaders))
	copy(out, newAPIUserIDHeaders)
	return out
}
