package collection

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

var ErrChannelReadBusy = errors.New("该渠道正在采集、验证 Cookie 或同步 Key，请稍后重试")

// CookieAccess 只使用用户登记的会话；不登录，不更新 Cookie，不执行远端写操作。
type CookieAccess struct {
	Pool        *store.Pool
	Credentials *store.CookieCredentialStore
	Client      *collector.Client
	http        *http.Client
}

func NewCookieAccess(pool *store.Pool, credentials *store.CookieCredentialStore, client *collector.Client) *CookieAccess {
	hc := http.Client{Timeout: 30 * time.Second}
	if client.HC != nil {
		hc = *client.HC
	}
	if hc.Timeout <= 0 || hc.Timeout > 30*time.Second {
		hc.Timeout = 30 * time.Second
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Cookie 不经过环境代理：否则代理重新解析 DNS，可以绕过下面的目标检查。
	transport.Proxy = nil
	transport.DialContext = dialCookie
	hc.Transport, hc.Jar = transport, nil
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	access := &CookieAccess{Pool: pool, Credentials: credentials, Client: client, http: &hc}
	client.CookieRequest = access.Do
	return access
}

func dialCookie(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, store.ErrCookieSite
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	for _, ip := range addresses {
		if !store.IsPublicCookieIP(ip) {
			return nil, store.ErrCookieSite
		}
	}
	dialer := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	for _, ip := range addresses {
		// 使用已经检查的 IP 建连，不让第二次 DNS 查询改变目标。
		conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		err = dialErr
	}
	if err == nil {
		err = errors.New("未解析到 Cookie 站点地址")
	}
	return nil, err
}

// cookieReadPath 同时限制 origin、路径、方法和查询参数，不提供任意 URL/头注入。
func cookieReadPath(req *http.Request, base string) (string, error) {
	origin, err := store.CookieOrigin(base)
	if err != nil {
		return "", err
	}
	target, err := cookieTargetOrigin(req)
	if err != nil || target != origin {
		return "", store.ErrCookieSite
	}
	if req.ContentLength != 0 || req.Body != nil && req.Body != http.NoBody {
		return "", store.ErrCookieSite
	}
	b, _ := url.Parse(base)
	prefix := strings.TrimRight(b.Path, "/")
	if !strings.HasPrefix(req.URL.Path, prefix+"/api/") {
		return "", store.ErrCookieSite
	}
	path := strings.TrimPrefix(req.URL.Path, prefix)
	if cookieEndpointAllowed(req, path) {
		return path, nil
	}
	return "", store.ErrCookieSite
}

func cookieTargetOrigin(req *http.Request) (string, error) {
	u := req.URL
	if u == nil {
		return "", store.ErrCookieSite
	}
	if u.User != nil || u.Fragment != "" || u.RawPath != "" || u.Opaque != "" {
		return "", store.ErrCookieSite
	}
	if req.Host != "" && req.Host != u.Host {
		return "", store.ErrCookieSite
	}
	return u.Scheme + "://" + strings.ToLower(u.Host), nil
}

func cookieEndpointAllowed(req *http.Request, path string) bool {
	switch req.Method {
	case http.MethodGet:
		switch path {
		case "/api/user/self", "/api/pricing":
			return req.URL.RawQuery == ""
		case "/api/token", "/api/token/":
			return cookieKeyQueryAllowed(req.URL.RawQuery)
		}
	case http.MethodPost:
		if req.URL.RawQuery != "" {
			return false
		}
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/token/"), "/key")
		n, err := strconv.ParseInt(id, 10, 64)
		return err == nil && n > 0 && path == "/api/token/"+strconv.FormatInt(n, 10)+"/key"
	}
	return false
}

func cookieKeyQueryAllowed(raw string) bool {
	query, err := url.ParseQuery(raw)
	if err != nil {
		return false
	}
	for key, values := range query {
		if key != "p" && key != "size" || len(values) != 1 {
			return false
		}
		n, err := strconv.ParseInt(values[0], 10, 64)
		if err != nil || n <= 0 || key == "size" && n > 100 {
			return false
		}
	}
	return true
}

func (a *CookieAccess) Do(req *http.Request, session collector.Session) (*http.Response, error) {
	if !session.CookieAllowed || session.Family != collector.FamilyNewAPI {
		return nil, collector.ErrCookieUnavailable
	}
	path, err := cookieReadPath(req, session.BaseURL)
	if err != nil {
		return nil, err
	}
	if path == "/api/token" {
		// 首站实测该读取地址会补尾斜杠；直接使用规范路径，不放开重定向。
		req = req.Clone(req.Context())
		req.URL.Path += "/"
	}
	if session.CookieState == nil {
		session.CookieState = &collector.CookieSession{}
	}
	if path != "/api/user/self" && session.CookieState.Revision.IsZero() {
		self, err := http.NewRequestWithContext(req.Context(), http.MethodGet, strings.TrimRight(session.BaseURL, "/")+"/api/user/self", nil)
		if err != nil {
			return nil, store.ErrCookieSite
		}
		resp, err := a.send(self, session, false)
		if err != nil {
			return nil, err
		}
		_ = resp.Body.Close()
	}
	return a.send(req, session, false)
}

func (a *CookieAccess) load(ctx context.Context, id int64) (store.CookieCredential, error) {
	conn, release, err := a.Pool.Acquire(ctx)
	if err != nil {
		return store.CookieCredential{}, errors.New("读取 Cookie 配置失败")
	}
	defer release()
	return a.Credentials.Load(ctx, conn, id)
}

func (a *CookieAccess) mark(ctx context.Context, cred store.CookieCredential, state string) error {
	conn, release, err := a.Pool.Acquire(ctx)
	if err != nil {
		return errors.New("更新 Cookie 状态失败")
	}
	defer release()
	return a.Credentials.MarkState(ctx, conn, cred, state)
}

func (a *CookieAccess) send(req *http.Request, s collector.Session, explicit bool) (*http.Response, error) {
	path, err := cookieReadPath(req, s.BaseURL)
	if err != nil {
		return nil, err
	}
	s.UserIDHeader = cmp.Or(s.CookieState.UserIDHeader, s.UserIDHeader, "New-API-User")
	candidates := []string{s.UserIDHeader}
	if path == "/api/user/self" && s.CookieState.Revision.IsZero() {
		for _, header := range collector.NewAPIUserIDHeaderCandidates() {
			if header != s.UserIDHeader {
				candidates = append(candidates, header)
			}
		}
	}
	var revision time.Time
	for {
		s.UserIDHeader = candidates[0]
		resp, cred, err := a.sendOnce(req, s, explicit, revision)
		if collector.IsAuthenticationFailure(err) && len(candidates) > 1 {
			// 探测期间只接受同一版 Cookie；全部候选失败后才标记失效。
			revision = cred.UpdatedAt
			candidates = candidates[1:]
			continue
		}
		return resp, a.reject(req.Context(), cred, err)
	}
}

func (a *CookieAccess) sendOnce(req *http.Request, s collector.Session, explicit bool, revision time.Time) (*http.Response, store.CookieCredential, error) {
	if err := a.Client.Wait(req.Context(), req.URL.Host); err != nil {
		return nil, store.CookieCredential{}, err
	}
	// 限速等待结束后才读配置，关闭、清除或替换后不再发送旧 Cookie。
	cred, err := a.load(req.Context(), s.AccountID)
	if err != nil {
		return nil, cred, err
	}
	req, path, err := a.prepare(req, cred, s, explicit, revision)
	if err != nil {
		return nil, cred, err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, cred, cookieNetworkError(err)
	}
	m, raw, err := collector.ReadAuthResponse(resp, req.Method, path, true)
	if err != nil {
		return nil, cred, err
	}
	if path == "/api/user/self" {
		userID := req.Header.Get(s.UserIDHeader)
		if !collector.CookieIdentityMatches(m, userID) {
			return nil, cred, errors.Join(collector.ErrCookieNeedsAction, errors.New("登记账号与 Cookie 用户 ID 不一致"))
		}
		revision, err := a.confirmIdentity(req.Context(), cred, userID)
		if err != nil {
			return nil, cred, err
		}
		s.CookieState.Revision, s.CookieState.ExternalUserID = revision, userID
		s.CookieState.UserIDHeader = s.UserIDHeader
	}
	// ponytail: 复用现有 JSON 解析入口，成功响应再解析一次；不用新增通用响应缓存。
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	return resp, cred, nil
}

func (a *CookieAccess) prepare(req *http.Request, cred store.CookieCredential, s collector.Session, explicit bool, revision time.Time) (*http.Request, string, error) {
	path, err := cookieReadPath(req, cred.BaseURL)
	if err != nil {
		return nil, "", err
	}
	if !revision.IsZero() && !revision.Equal(cred.UpdatedAt) {
		return nil, "", store.ErrCookieCredentialChanged
	}
	if err := checkCookieSession(cred, s, explicit); err != nil {
		return nil, "", err
	}
	headers, err := cookieHeaders(cred, s.UserIDHeader)
	if err != nil {
		return nil, "", err
	}
	req = req.Clone(req.Context())
	req.Header = headers
	return req, path, nil
}

func checkCookieSession(cred store.CookieCredential, s collector.Session, explicit bool) error {
	userID := s.ExternalUserID
	if userID == "" {
		userID = s.CookieState.ExternalUserID
	}
	if s.BaseURL != cred.BaseURL || userID != cred.ExternalUserID {
		return store.ErrCookieCredentialChanged
	}
	if !s.CookieState.Revision.IsZero() && !s.CookieState.Revision.Equal(cred.UpdatedAt) {
		return store.ErrCookieCredentialChanged
	}
	if explicit {
		return nil
	}
	switch cred.State {
	case "expired":
		return collector.ErrCookieExpired
	case "needs_action":
		return collector.ErrCookieNeedsAction
	}
	return nil
}

func cookieHeaders(cred store.CookieCredential, header string) (http.Header, error) {
	if cred.ExternalUserID == "" {
		var err error
		cred.ExternalUserID, err = cookieUserID(cred.CookieHeader)
		if err != nil {
			return nil, err
		}
	}
	uid, err := strconv.ParseInt(cred.ExternalUserID, 10, 64)
	if err != nil || uid <= 0 {
		return nil, collector.ErrCookieNeedsAction
	}
	if header == "" {
		header = "New-API-User"
	}
	if !slices.Contains(collector.NewAPIUserIDHeaderCandidates(), header) {
		return nil, collector.ErrCookieNeedsAction
	}
	headers := http.Header{"Accept": {"application/json"}, "Cookie": {cred.CookieHeader}}
	headers.Set(header, cred.ExternalUserID)
	return headers, nil
}

func (a *CookieAccess) reject(ctx context.Context, cred store.CookieCredential, err error) error {
	state := ""
	if collector.IsAuthenticationFailure(err) {
		state = "expired"
		err = errors.Join(collector.ErrCookieExpired, err)
	} else if errors.Is(err, collector.ErrCookieNeedsAction) {
		state = "needs_action"
	}
	if state != "" {
		if updateErr := a.mark(ctx, cred, state); updateErr != nil {
			return updateErr
		}
	}
	return err
}

func cookieNetworkError(err error) error {
	if errors.Is(err, store.ErrCookieSite) {
		return store.ErrCookieSite
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return context.DeadlineExceeded
	}
	return errors.New("无法连接 Cookie 站点，请检查站点与网络")
}

// Validate 只使用已存 Cookie，不能用有效令牌掩盖 Cookie 失效。
func (a *CookieAccess) Validate(ctx context.Context, accountID int64) error {
	ctx, cancel := context.WithTimeout(ctx, defaultChannelSyncTimeout)
	defer cancel()
	conn, release, err := a.Pool.Acquire(ctx)
	if err != nil {
		return errors.New("读取 Cookie 配置失败")
	}
	defer release()
	var channelID int64
	if err := conn.QueryRow(ctx, `SELECT channel_id FROM upstream_accounts WHERE id=$1`, accountID).Scan(&channelID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrNotFound
		}
		return errors.New("读取 Cookie 账号失败")
	}
	unlock, err := lockChannelRead(ctx, conn, channelID)
	if err != nil {
		return err
	}
	defer unlock()
	cred, err := a.Credentials.Load(ctx, conn, accountID)
	if err != nil {
		return err
	}
	s := collector.SessionFrom(collector.Credential{
		AccountID: accountID, ChannelID: channelID, Family: collector.FamilyNewAPI,
		ExternalUserID: cred.ExternalUserID, CookieEnabled: true,
	}, cred.BaseURL, 0)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cred.BaseURL, "/")+"/api/user/self", nil)
	if err != nil {
		return store.ErrCookieSite
	}
	resp, err := a.send(req, s, true)
	if err == nil {
		_ = resp.Body.Close()
	}
	return err
}

func lockChannelRead(ctx context.Context, conn *pgx.Conn, channelID int64) (func(), error) {
	locked, err := store.TryChannelSyncLock(ctx, conn, channelID)
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, ErrChannelReadBusy
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := store.UnlockChannelSync(ctx, conn, channelID); err != nil {
			slog.Error("释放渠道读取锁失败", "channel_id", channelID)
		}
	}, nil
}
