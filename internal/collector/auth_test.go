package collector

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memStore struct {
	mu     sync.Mutex
	saved  []Credential
	latest map[string]Credential
	err    error
	// onSave 在保存时回调，用于验证"先持久化再释放锁"的顺序
	onSave func()
}

func (m *memStore) WithRefreshLock(
	_ context.Context, c Credential,
	refresh func(Credential) (Credential, bool, error),
) (Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := RefreshLockKey(c)
	if cur, ok := m.latest[key]; ok {
		c = cur
	}
	fresh, changed, err := refresh(c)
	if err != nil {
		return c, err
	}
	if !changed {
		return fresh, nil
	}
	if m.onSave != nil {
		m.onSave()
	}
	if m.err != nil {
		return c, m.err
	}
	if m.latest == nil {
		m.latest = map[string]Credential{}
	}
	m.saved = append(m.saved, fresh)
	m.latest[key] = fresh
	return fresh, nil
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

// **登记进来的凭证也要能判定续期。**
//
// 这条补的是上面那几条测不到的那一段：它们每一条都自己填了 TokenExpiresAt，
// 而**真实的登记路径从不填它** —— saveCredential 与导入侧构造 Credential 时
// 都没有这个字段（all-api-hub 导出里只有 access_token，没有到期时间也没有
// refresh_token），于是库里 13 条 sub2api 凭证的 token_expires_at 全是 NULL。
//
// 后果是 RefreshLead=120s 对登记进来的凭证是**死的**：NeedsRefresh 在零值时
// 返回 false，令牌到期后只能等某次采集撞上 401。而 401 之后 sync.go 直接
// return"鉴权失败"，没有反应式补救 —— 也就是说主动续期只在"已经续过一次
// 之后"才生效，那是个自锁。
//
// 2026-08-30 实测：对 65 个渠道跑全量 sync，core 日志里续期动作 **0 次**，
// 13 个 sub2api 全部 401。库里那些 JWT 的 exp 声明分别是 2026-03（8 条，
// 五个月前就过期）与 2026-08-27~29（3 条）—— **到期时间一直写在令牌里，
// 只是没人读**。
func TestRegisteredCredentialCanStillDecideRefresh(t *testing.T) {
	// 形态与 saveCredential / 导入侧构造出来的完全一致：只有 access_token，
	// 没有 TokenExpiresAt。exp 取一个已过去的时刻，故"需要续期"是正确答案。
	tok := testJWT(t, map[string]any{
		"exp":   time.Now().Add(-2 * time.Hour).Unix(),
		"email": "x@example.com",
	})
	cred := Credential{
		Family: FamilySub2API, ChannelID: 1, CredType: "sub2api_jwt",
		AccessToken: tok,
		// TokenExpiresAt 刻意留零值 —— 这正是登记路径的产物
	}
	if !NeedsRefresh(cred, time.Now()) {
		t.Fatal("登记进来的 sub2api 凭证（只有 access_token、无 TokenExpiresAt）" +
			"判定为不需续期 —— 令牌里的 exp 没人读，于是 RefreshLead=120s 形同虚设，" +
			"到期后只能等某次采集撞 401，而 401 之后没有反应式补救")
	}
	// 对照：exp 还很远时不该刷
	fresh := cred
	fresh.AccessToken = testJWT(t, map[string]any{
		"exp": time.Now().Add(10 * time.Hour).Unix(),
	})
	if NeedsRefresh(fresh, time.Now()) {
		t.Error("exp 还有 10 小时却判定需要续期 —— 会每次采集都刷一遍，" +
			"而刷新会轮换 refresh_token")
	}
}

// NewAPI 的令牌不是 JWT，也没有 exp。它必须继续走"永不主动续期"那条路：
// 不变式 N-1 —— /api/user/token 是重新生成而非读取，主动刷新会踢掉正在用的令牌。
func TestNewAPIStillNeverRefreshesEvenWithJWTLikeToken(t *testing.T) {
	cred := Credential{
		Family: FamilyNewAPI, ChannelID: 1,
		// 就算有人把一个已过期的 JWT 当 NewAPI 令牌登记进来
		AccessToken: testJWT(t, map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}),
	}
	if NeedsRefresh(cred, time.Now()) {
		t.Fatal("NewAPI 判定需要续期 —— 不变式 N-1 被破：" +
			"RefreshLead=0 必须优先于任何从令牌读出的到期时间")
	}
}

// 不是 JWT 的 access_token 不得让判定崩掉或误判。
//
// 真库里有两条这样的（渠道 30/31：cred_type 是 sub2api_jwt 而内容不是三段
// JWT）。读不出 exp 时**只能退回"不主动续期"** —— 猜一个默认到期时间会让
// 它在那个凭空的时刻到来后去刷一个真实有效期未知的令牌，而那两条连
// refresh_token 都没有，结果是一个读起来像"凭证坏了"的 fatal。
//
// ⚠️ **必须直接断言 jwtExpiry 的 ok，不能只透过 NeedsRefresh 看。**
// 第一版只有下面那个 NeedsRefresh 循环，于是"读不出时编一个 now+24h"这种
// 改法**照旧全绿**：编出来的是未来时刻，NeedsRefresh 同样返回 false。
// 破坏性验证当场撞上了这一点（破坏 C 变绿）。纯函数要直接测纯函数 ——
// 同 TestAsFloatToleratesStringNumbers 的教训。
func TestNonJWTTokenFallsBackToNoRefresh(t *testing.T) {
	bad := []struct{ name, tok string }{
		{"空串", ""},
		{"不含点", "not-a-jwt"},
		{"只有两段", "a.b"},
		{"四段", "a.b.c.d"},
		{"payload 不是 base64", "a.!!!不是 base64!!!.c"},
		{"payload 不是 JSON",
			"a." + base64.RawURLEncoding.EncodeToString([]byte("{不是 JSON")) + ".c"},
		{"没有 exp 声明",
			"a." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"x"}`)) + ".c"},
		{"exp 是 0",
			"a." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":0}`)) + ".c"},
		{"exp 是负数",
			"a." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":-1}`)) + ".c"},
		{"exp 是字符串",
			"a." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":"123"}`)) + ".c"},
	}
	for _, c := range bad {
		// ① 直接测：必须明确报"读不出"，不许返回任何时刻
		if got, ok := jwtExpiry(c.tok); ok {
			t.Errorf("%s：jwtExpiry 竟读出了 %v —— 读不出就得返回 false，"+
				"编一个默认到期会让系统按凭空的节奏刷令牌", c.name, got)
		}
		// ② 再测经由 NeedsRefresh 的行为（这一层看不出上面那种编造，故两层都要）
		cred := Credential{Family: FamilySub2API, ChannelID: 1, AccessToken: c.tok}
		if NeedsRefresh(cred, time.Now()) {
			t.Errorf("%s：读不出 exp 却判定需要续期", c.name)
		}
	}

	// 正向对照：带填充的 base64url 也要吃下来（有些实现留着 '='）。
	// 只认无填充会让"看着像 JWT 的令牌"静默读不出 exp。
	//
	// 明文长度必须**不是 3 的倍数**才会产生 '=' 填充。这里用循环凑，而不是
	// 手挑一个长度：第一版直接编 `{"exp":<10位>}` 得到 24 字节（整除 3），
	// 于是根本没有填充 —— 那条用例就在测另一件事了。下面的自检当场抓到它。
	want := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	var payload string
	for pad := 0; pad < 3; pad++ {
		raw := `{"exp":` + strconv.FormatInt(want.Unix(), 10) +
			`,"x":"` + strings.Repeat("y", pad) + `"}`
		payload = base64.URLEncoding.EncodeToString([]byte(raw))
		if strings.Contains(payload, "=") {
			break
		}
	}
	if !strings.Contains(payload, "=") {
		t.Fatalf("造不出带填充的 payload：%q", payload)
	}
	got, ok := jwtExpiry("a." + payload + ".c")
	if !ok || !got.Equal(want) {
		t.Errorf("带 '=' 填充的 payload 读不出 exp：got=%v ok=%v，期望 %v", got, ok, want)
	}
}

// testJWT 拼一个只有 payload 有意义的三段串。
//
// 造的是**一个令牌的字节**，不是站点：被测对象是"我方能不能从自己库里已存的
// 令牌读出 exp"，而真站点不会按需签发一个"两小时前就过期"的令牌
// （CLAUDE.md §1 例外判据）。签名段是占位符 —— 这里不验签，只读 claim，
// 因为读的是我方自己存进去的令牌，用途仅是决定何时续期。
func testJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	b, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("造 JWT payload: %v", err)
	}
	return "eyJhbGciOiJIUzI1NiJ9." +
		base64.RawURLEncoding.EncodeToString(b) + ".sig-not-verified"
}

func TestRefreshLockKeyUsesSub2APIAccountIdentity(t *testing.T) {
	tok := testJWT(t, map[string]any{"user_id": 42, "exp": time.Now().Add(time.Hour).Unix()})
	first := Credential{
		Family: FamilySub2API, ChannelID: 1, BaseURL: "HTTPS://API.EXAMPLE.COM/",
		AccessToken: tok,
	}
	withStoredID := first
	withStoredID.ExternalUserID = "7500"
	if got := RefreshLockKey(withStoredID); !strings.HasSuffix(got, ":7500") {
		t.Fatalf("已有 external_user_id 时应优先于 JWT user_id，得到 %q", got)
	}
	second := first
	second.ChannelID = 2
	second.BaseURL = "https://api.example.com"
	if a, b := RefreshLockKey(first), RefreshLockKey(second); a == "" || a != b {
		t.Fatalf("同站同账号应共享刷新锁键，得到 %q 与 %q", a, b)
	}

	other := second
	other.AccessToken = testJWT(t, map[string]any{"user_id": 43})
	if RefreshLockKey(other) == RefreshLockKey(first) {
		t.Fatal("同站不同账号不得共享刷新锁键")
	}
	if got := RefreshLockKey(Credential{Family: FamilyNewAPI, ChannelID: 1}); got != "" {
		t.Fatalf("无需主动刷新的 NewAPI 不应生成刷新锁键，得到 %q", got)
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
	store.err = nil
	got, err := auth.EnsureFresh(context.Background(), cred, r, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if r.calls.Load() != 2 || got.AccessToken != "new-access" {
		t.Errorf("未落库的凭证不应被复用：calls=%d access=%q", r.calls.Load(), got.AccessToken)
	}
}
