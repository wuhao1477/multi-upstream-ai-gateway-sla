package collector

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memStore struct {
	mu    sync.Mutex
	saved []Credential
	err   error
	// onSave 在保存时回调，用于验证"先持久化再释放锁"的顺序
	onSave func()
}

func (m *memStore) Save(_ context.Context, c Credential) error {
	if m.onSave != nil {
		m.onSave()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.saved = append(m.saved, c)
	return nil
}

func (m *memStore) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.saved)
}

// countingRefresher 记录刷新次数，并轮换 refresh_token（模拟真实行为）。
type countingRefresher struct {
	calls atomic.Int32
	delay time.Duration
	err   error
}

func (r *countingRefresher) Refresh(_ context.Context, c Credential) (Credential, error) {
	r.calls.Add(1)
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	if r.err != nil {
		return c, r.err
	}
	c.AccessToken = "new-access"
	c.RefreshToken = "rotated-refresh" // 真实 refresh 会轮换它
	c.TokenExpiresAt = time.Now().Add(24 * time.Hour)
	return c, nil
}

// ── 不变式 N-1：NewAPI 长期令牌永不主动刷新 ──
//
// 若主动刷新，就意味着调用 /api/user/token，而它会**作废正在使用的令牌**。
func TestNewAPINeverRefreshes(t *testing.T) {
	cred := Credential{
		Family: FamilyNewAPI, ChannelID: 1,
		// 即便令牌"已过期"，也不该主动刷新
		TokenExpiresAt: time.Now().Add(-time.Hour),
	}
	if NeedsRefresh(cred, time.Now()) {
		t.Fatal("NewAPI 长期令牌不得主动刷新（不变式 N-1：/api/user/token " +
			"是重新生成而非读取，会踢掉正在使用的令牌）")
	}
}

func TestSub2APIRefreshesBeforeExpiry(t *testing.T) {
	now := time.Now()
	// 还有 5 分钟到期 → 超出 120s 缓冲，不需要刷
	notYet := Credential{Family: FamilySub2API, TokenExpiresAt: now.Add(5 * time.Minute)}
	if NeedsRefresh(notYet, now) {
		t.Error("距到期 5 分钟不应刷新")
	}
	// 还有 60s → 进入 120s 缓冲，需要刷
	soon := Credential{Family: FamilySub2API, TokenExpiresAt: now.Add(60 * time.Second)}
	if !NeedsRefresh(soon, now) {
		t.Error("距到期 60s 应刷新（缓冲 120s）")
	}
}

// 未注册的家族一律不主动续期，**不能回退到某个默认阈值**。
//
// 这是 NeedsRefresh 唯一的兜底分支。给它一个非零默认值会让"漏注册"
// 变成"按别人的阈值刷别人的端点"，比不刷更糟。
func TestUnregisteredFamilyNeverRefreshes(t *testing.T) {
	now := time.Now()
	cred := Credential{Family: Family("某个还没注册的站"), TokenExpiresAt: now.Add(time.Second)}
	if NeedsRefresh(cred, now) {
		t.Error("未注册的家族不该判定需要续期 —— 没有注册就没有续期端点")
	}
}

// ── 不变式 S-1：同账号刷新必须串行 ──
//
// 这是本文件最重要的测试：并发刷新会互相作废 refresh_token。
func TestSub2APIConcurrentRefreshIsSerialized(t *testing.T) {
	store := &memStore{}
	auth := NewAuthenticator(store)
	// 加延迟放大竞态窗口 —— 没有锁的话必然多次刷新
	r := &countingRefresher{delay: 30 * time.Millisecond}

	cred := Credential{
		Family: FamilySub2API, ChannelID: 7,
		AccessToken: "old", RefreshToken: "old-refresh",
		TokenExpiresAt: time.Now().Add(10 * time.Second), // 已进缓冲
	}

	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := auth.EnsureFresh(context.Background(), cred, r, time.Now())
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个 goroutine 失败: %v", i, err)
		}
	}
	// 关键断言：10 个并发只应发生**一次**刷新。
	// 多于 1 意味着并发刷新，真实环境下后到的会作废先到的 refresh_token。
	if got := r.calls.Load(); got != 1 {
		t.Fatalf("刷新次数 = %d，期望 1（不变式 S-1：同账号刷新必须串行，"+
			"并发刷新会互相作废 refresh_token）", got)
	}
	if store.count() != 1 {
		t.Errorf("持久化次数 = %d，期望 1", store.count())
	}
}

// 不同渠道之间不该互相阻塞 —— 锁是按账号的，不是全局的。
func TestDifferentChannelsRefreshInParallel(t *testing.T) {
	auth := NewAuthenticator(&memStore{})
	r := &countingRefresher{}

	var wg sync.WaitGroup
	for ch := int64(1); ch <= 5; ch++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			cred := Credential{
				Family: FamilySub2API, ChannelID: id,
				TokenExpiresAt: time.Now().Add(10 * time.Second),
			}
			if _, err := auth.EnsureFresh(context.Background(), cred, r, time.Now()); err != nil {
				t.Errorf("渠道 %d: %v", id, err)
			}
		}(ch)
	}
	wg.Wait()

	// 5 个不同渠道各刷一次
	if got := r.calls.Load(); got != 5 {
		t.Errorf("刷新次数 = %d，期望 5（每渠道一次）", got)
	}
}

// **先持久化再释放锁**：持久化失败必须报错，不能静默返回新凭证。
//
// 若静默返回：内存里是新 refresh_token、库里还是旧的，
// 下次进程重启后用旧 token 刷新必然失败（它已被这次刷新作废）。
func TestPersistFailureIsReported(t *testing.T) {
	store := &memStore{err: errors.New("磁盘满")}
	auth := NewAuthenticator(store)
	r := &countingRefresher{}

	cred := Credential{
		Family: FamilySub2API, ChannelID: 3,
		TokenExpiresAt: time.Now().Add(10 * time.Second),
	}
	_, err := auth.EnsureFresh(context.Background(), cred, r, time.Now())
	if err == nil {
		t.Fatal("持久化失败必须报错——否则库中留着已被作废的 refresh_token，" +
			"下次重启后刷新必然失败")
	}
}

func TestRefreshErrorPropagates(t *testing.T) {
	auth := NewAuthenticator(&memStore{})
	r := &countingRefresher{err: ErrNeedsRelogin}

	cred := Credential{
		Family: FamilySub2API, ChannelID: 4,
		TokenExpiresAt: time.Now().Add(10 * time.Second),
	}
	_, err := auth.EnsureFresh(context.Background(), cred, r, time.Now())
	if !errors.Is(err, ErrNeedsRelogin) {
		t.Fatalf("应透出 ErrNeedsRelogin，得到 %v", err)
	}
}

// 不需要刷新时不该动 refresher，也不该持久化。
func TestNoRefreshWhenFresh(t *testing.T) {
	store := &memStore{}
	auth := NewAuthenticator(store)
	r := &countingRefresher{}

	cred := Credential{
		Family: FamilySub2API, ChannelID: 5,
		TokenExpiresAt: time.Now().Add(time.Hour),
	}
	got, err := auth.EnsureFresh(context.Background(), cred, r, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if r.calls.Load() != 0 {
		t.Error("凭证新鲜时不该刷新")
	}
	if store.count() != 0 {
		t.Error("未刷新时不该持久化")
	}
	if got.AccessToken != cred.AccessToken {
		t.Error("未刷新时应原样返回")
	}
}

// NewAPI 二开的头名 fan-out 列表必须完整（04 §3.1 实测七个）。
func TestNewAPIUserIDHeaderCandidates(t *testing.T) {
	got := NewAPIUserIDHeaderCandidates()
	if len(got) != 7 {
		t.Fatalf("候选头名 = %d 个，期望 7（04 §3.1）", len(got))
	}
	if got[0] != "New-API-User" {
		t.Errorf("首个候选应是官方头名 New-API-User，得到 %q", got[0])
	}
	// 返回副本，调用方改动不该影响内部状态
	got[0] = "tampered"
	if NewAPIUserIDHeaderCandidates()[0] != "New-API-User" {
		t.Error("应返回副本，避免调用方污染候选列表")
	}
}

func TestSessionFrom(t *testing.T) {
	cred := Credential{
		ChannelID: 9, Family: FamilyNewAPI, AccessToken: "tok",
		UserIDHeaderName: "Veloera-User", ExternalUserID: "42",
	}
	s := SessionFrom(cred, "https://x.example/", 500000)
	if s.Token != "tok" || s.UserIDHeader != "Veloera-User" || s.ExternalUserID != "42" {
		t.Errorf("字段未正确传递: %+v", s)
	}
	if s.QuotaPerUnit != 500000 {
		t.Errorf("quota_per_unit = %v", s.QuotaPerUnit)
	}
}

// 高并发下不变式 S-1 仍成立（100 个 goroutine）。
// 数量拉大是为了让"锁内 double-check 读错对象"这类缺陷必然暴露 ——
// 首版就是读了调用方自己的旧副本，10 并发刷了 10 次。
func TestInvariantS1UnderHeavyContention(t *testing.T) {
	store := &memStore{}
	auth := NewAuthenticator(store)
	r := &countingRefresher{delay: 5 * time.Millisecond}

	cred := Credential{
		Family: FamilySub2API, ChannelID: 42,
		AccessToken: "old", RefreshToken: "old-refresh",
		TokenExpiresAt: time.Now().Add(10 * time.Second),
	}

	const n = 100
	var wg sync.WaitGroup
	tokens := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			got, err := auth.EnsureFresh(context.Background(), cred, r, time.Now())
			if err != nil {
				t.Errorf("goroutine %d: %v", idx, err)
				return
			}
			tokens[idx] = got.AccessToken
		}(i)
	}
	wg.Wait()

	if got := r.calls.Load(); got != 1 {
		t.Fatalf("100 并发下刷新 %d 次，期望 1", got)
	}
	// 所有调用方都必须拿到**同一个**新令牌 —— 拿到旧令牌的那个会用作废的凭证去采集
	for i, tok := range tokens {
		if tok != "new-access" {
			t.Fatalf("goroutine %d 拿到 %q，期望所有人都拿到刷新后的令牌", i, tok)
		}
	}
}

// 持久化失败时**不得**登记为最新：否则后到者拿到一个库里没有的凭证，
// 进程重启后无从恢复，而真实的 refresh_token 已经被轮换掉了。
func TestFailedPersistDoesNotBecomeLatest(t *testing.T) {
	store := &memStore{err: errors.New("写库失败")}
	auth := NewAuthenticator(store)
	r := &countingRefresher{}

	cred := Credential{
		Family: FamilySub2API, ChannelID: 8,
		AccessToken:    "old",
		TokenExpiresAt: time.Now().Add(10 * time.Second),
	}
	if _, err := auth.EnsureFresh(context.Background(), cred, r, time.Now()); err == nil {
		t.Fatal("持久化失败应报错")
	}
	// 第二次调用应重新尝试刷新（而不是复用那个没落库的凭证）
	if got := auth.newestKnown(cred).AccessToken; got != "old" {
		t.Errorf("未落库的凭证不该成为 latest，当前 %q", got)
	}
}
