package collector

import (
	"context"
	"errors"
	"fmt"
	"sync"
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

// refreshBuffer 是"到期前多久主动刷新"的阈值。
// Sub2API 的官方前端用 120s（SUB2API_TOKEN_REFRESH_BUFFER_MS，04 §5.2），
// 这里沿用 —— 太短会在刷新失败时没有重试余量。
const refreshBuffer = 120 * time.Second

// asxsReloginThreshold：ASXS 的 JWT 有效期 7 天且**无 refresh 路径**，
// 只能账密重登（04 §5.3）。剩余不足 1 天即重登，频率约每周一次、风控压力小。
const asxsReloginThreshold = 24 * time.Hour

// CredentialStore 是凭证持久化契约。
//
// 抽成接口而非直接依赖 store 包：collector 不该反向依赖存储层的具体实现，
// 且这样才能对"刷新结果先持久化再释放锁"做单测（04 §5.2 不变式 S-1）。
type CredentialStore interface {
	// Save 持久化凭证。必须在**释放刷新锁之前**完成（不变式 S-1）。
	Save(ctx context.Context, cred Credential) error
}

// Authenticator 按站型执行鉴权与续期，并保证两条硬约束。
type Authenticator struct {
	Store CredentialStore

	// mu 保护 locks 与 latest 两个 map。
	mu sync.Mutex
	// locks 是**按账号**的刷新互斥锁（不变式 S-1）。
	// 键用 (family, channelID)：同一渠道的凭证共享一把锁。
	locks map[string]*sync.Mutex
	// latest 是每个账号**最近一次刷新后**的凭证。
	//
	// ⚠️ 没有它，不变式 S-1 只做到一半（本包测试抓到）：锁只保证串行**进入**，
	// 而锁内的 double-check 读的是调用方自己那份 cred —— 每个 goroutine 各持
	// 一份旧副本，判定永远是"需要刷新"，于是 10 个并发照样刷 10 次，
	// 真实环境下后 9 次会互相作废 refresh_token。
	// 串行化的目的不是排队，而是**让后到者看见先到者的结果**。
	latest map[string]Credential
}

// NewAuthenticator 构造鉴权器。
func NewAuthenticator(store CredentialStore) *Authenticator {
	return &Authenticator{
		Store:  store,
		locks:  map[string]*sync.Mutex{},
		latest: map[string]Credential{},
	}
}

func credKey(cred Credential) string {
	return fmt.Sprintf("%s:%d", cred.Family, cred.ChannelID)
}

// lockFor 取某账号的刷新锁。
func (a *Authenticator) lockFor(cred Credential) *sync.Mutex {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.locks == nil {
		a.locks = map[string]*sync.Mutex{}
	}
	key := credKey(cred)
	l, ok := a.locks[key]
	if !ok {
		l = &sync.Mutex{}
		a.locks[key] = l
	}
	return l
}

// newestKnown 返回该账号已知最新的凭证；无记录则返回传入值。
func (a *Authenticator) newestKnown(cred Credential) Credential {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.latest[credKey(cred)]; ok {
		return c
	}
	return cred
}

// remember 记录刷新结果，供后到者的 double-check 使用。
func (a *Authenticator) remember(cred Credential) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.latest == nil {
		a.latest = map[string]Credential{}
	}
	a.latest[credKey(cred)] = cred
}

// Refresher 是站型特定的续期实现。
type Refresher interface {
	// Refresh 用当前凭证换取新凭证。
	// Sub2API：POST /api/v1/auth/refresh，会轮换 refresh_token。
	// ASXS：POST /api/manage/auth/login，账密重登。
	// NewAPI：不实现（长期令牌，不变式 N-1 禁止重新生成）。
	Refresh(ctx context.Context, cred Credential) (Credential, error)
}

// NeedsRefresh 判断凭证是否需要续期。
//
// 三家族策略不同（04 §5）：
//   - NewAPI：长期令牌，**永不主动刷新**（不变式 N-1）。只有 401 才说明
//     被后台重置了，那时需人工介入。
//   - Sub2API：到期前 120s 主动刷新。
//   - ASXS：剩余 < 1 天即重登（无 refresh 路径，只能账密）。
func NeedsRefresh(cred Credential, now time.Time) bool {
	switch cred.Family {
	case FamilyNewAPI:
		return false
	case FamilySub2API:
		if cred.TokenExpiresAt.IsZero() {
			return false
		}
		return now.Add(refreshBuffer).After(cred.TokenExpiresAt)
	case FamilyASXS:
		if cred.TokenExpiresAt.IsZero() {
			return false
		}
		return now.Add(asxsReloginThreshold).After(cred.TokenExpiresAt)
	}
	return false
}

// EnsureFresh 在需要时续期凭证，返回可用的凭证。
//
// **不变式 S-1 的落地**：整个"判定 → 刷新 → 持久化"在账号锁内完成。
// 锁内二次判定（double-check）是必要的：等锁期间前一个持有者可能已经刷过了，
// 此时再刷一次会作废刚拿到的 refresh_token —— 那正是并发刷新互相作废的场景。
func (a *Authenticator) EnsureFresh(
	ctx context.Context, cred Credential, r Refresher, now time.Time,
) (Credential, error) {
	// 快速路径：先看本账号已知最新的凭证够不够新，避免无谓抢锁。
	if cur := a.newestKnown(cred); !NeedsRefresh(cur, now) {
		return cur, nil
	}

	lock := a.lockFor(cred)
	lock.Lock()
	defer lock.Unlock()

	// 锁内二次判定：等锁期间前一个持有者很可能已经刷过了。
	// **必须读 newestKnown 而不是传入的 cred** —— 每个调用方各持一份旧副本，
	// 读它的话判定恒为"需要刷新"，串行化就只剩排队、防不住重复刷新
	// （不变式 S-1 的真正目的是让后到者看见先到者的结果）。
	cur := a.newestKnown(cred)
	if !NeedsRefresh(cur, time.Now()) {
		return cur, nil
	}

	fresh, err := r.Refresh(ctx, cur)
	if err != nil {
		return cur, fmt.Errorf("续期失败（%s/渠道 %d）: %w",
			cred.Family, cred.ChannelID, err)
	}

	// **先持久化再释放锁**（defer 在函数返回时才释放，故此处顺序正确）。
	// 若先释放锁，下一个等待者会读到旧凭证又刷一次，把刚拿到的 refresh_token
	// 作废 —— 这正是不变式 S-1 要防的。
	if a.Store != nil {
		if err := a.Store.Save(ctx, fresh); err != nil {
			return cur, fmt.Errorf("续期成功但持久化失败（%s/渠道 %d）"+
				"—— 新 refresh_token 已生效而库中仍是旧的，下次刷新会失败: %w",
				cred.Family, cred.ChannelID, err)
		}
	}
	// 只在持久化成功后才登记：否则后到者会拿到一个"库里没有"的凭证，
	// 进程重启后无从恢复。
	a.remember(fresh)
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
