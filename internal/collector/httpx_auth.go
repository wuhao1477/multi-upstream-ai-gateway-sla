package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
)

// authHeaders 构造鉴权头。
//
// **这里没有逐家族分流，是刻意的。** 三家族的差别只有"要不要带用户 ID 头"，
// 而那由会话里有没有头名决定 —— 头名只有 NewAPI 的 Authenticate 会 fan-out
// 试探出来并写进凭证（04 §3.1：只带 Authorization 会 401，且二开站改了头名）。
// 原先这里是一个 switch，它表达的是**零个变化点**：三个 case 里两个逐字相同，
// 第三个多的那两行本身已经被 if 守住了。
//
// 这一处曾是"加站型要改九处"里最坏的两处之一：漏加 case 的后果是新站型
// 一个头都不带 → 401，而编译、单测、Capabilities 声明全是绿的。
// 删掉分流之后，新站型默认就拿到 Bearer + 有则带用户 ID 头 —— 漏不掉。
func authHeaders(s Session) http.Header {
	h := http.Header{}
	// 实测 `Authorization: <token>` 与 `Bearer <token>` 均可，用后者更通用
	if s.Token != "" {
		h.Set("Authorization", "Bearer "+s.Token)
	}
	if s.UserIDHeader != "" && s.ExternalUserID != "" {
		h.Set(s.UserIDHeader, s.ExternalUserID)
	}
	h.Set("Accept", "application/json")
	return h
}

// getJSONAuth 带鉴权取 JSON，返回解析后的 map 与原始字节。
func (c *Client) getJSONAuth(ctx context.Context, s Session, path string) (map[string]any, []byte, error) {
	return c.doJSONAuth(ctx, s, http.MethodGet, path)
}

func (c *Client) postJSONAuth(ctx context.Context, s Session, path string) (map[string]any, []byte, error) {
	return c.doJSONAuth(ctx, s, http.MethodPost, path)
}

func (c *Client) doJSONAuth(
	ctx context.Context, s Session, method, path string,
) (map[string]any, []byte, error) {
	return c.doJSONBodyAuth(ctx, s, method, path, nil)
}

func (c *Client) postJSONBodyAuth(
	ctx context.Context, s Session, path string, body any,
) (map[string]any, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, nil, fmt.Errorf("编码 %s 请求失败: %w", path, err)
	}
	return c.doJSONBodyAuth(ctx, s, http.MethodPost, path, raw)
}

func (c *Client) doJSONBodyAuth(
	ctx context.Context, s Session, method, path string, body []byte,
) (map[string]any, []byte, error) {
	url := strings.TrimRight(s.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header = authHeaders(s)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if s.CookieAllowed && (s.Token == "" || (s.CookieState != nil && s.CookieState.Active)) {
		return c.cookieJSON(req, s, path)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	m, raw, err := ReadAuthResponse(resp, method, path, false)
	if s.CookieAllowed && IsAuthenticationFailure(err) {
		return c.cookieJSON(req, s, path)
	}
	return m, raw, err
}

func (c *Client) cookieJSON(req *http.Request, s Session, path string) (map[string]any, []byte, error) {
	if c.CookieRequest == nil {
		return nil, nil, ErrCookieUnavailable
	}
	req.Header.Del("Authorization")
	if s.CookieState != nil {
		s.CookieState.Active = true
	}
	resp, err := c.CookieRequest(req, s)
	if err != nil {
		return nil, nil, err
	}
	return ReadAuthResponse(resp, req.Method, path, true)
}

// ReadAuthResponse 复用认证响应解析；Cookie 错误不返回正文或上游自定义消息。
func ReadAuthResponse(resp *http.Response, method, path string, secret bool) (m map[string]any, raw []byte, err error) {
	defer func() {
		if secret && err != nil {
			raw = nil
		}
	}()
	raw, err = readAuthBody(resp, secret)
	if err != nil {
		return nil, nil, fmt.Errorf("读 %s %s 响应: %w", method, path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, raw, authHTTPError(resp, method, path, raw, secret)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		if secret && path == CookieSessionPath {
			return nil, nil, fmt.Errorf("%s %s 非 JSON: %w", method, path, ErrCookieNeedsAction)
		}
		if secret {
			return nil, nil, fmt.Errorf("%s %s 非 JSON", method, path)
		}
		return nil, raw, fmt.Errorf("解析 %s %s 响应失败（非 JSON）: %s", method, path, snippet(raw))
	}
	// ⚠️ NewAPI 系用 **HTTP 200 + `{"success":false,"message":"…"}`** 表达失败，
	// 不是用状态码。上面那两个状态码判断一个都拦不住它，于是调用方拿到的是一个
	// "解析成功但没有 data" 的 map —— 而每个调用方对这种 map 的反应都是**静默给
	// 出空数据**：
	//   · fetchAllKeyItems：`data` 不存在 → 直接 return，**空 Key 列表且无错误**，
	//     表现为"这个账号没有 Key"（2026-09-16 用户报的正是这个症状）；
	//   · FetchAccount：`d["quota"]` 取不到 → 余额算成 **0 美元**，一个看起来
	//     精确的错数（FR-020 最忌讳的那种）；
	//   · Authenticate：`quota`/`id` 都没有 → 报"头名 X 通过但响应无预期字段"，
	//     把矛头指向用户 ID 头，而上游明明白白写着是令牌的问题。
	//
	// 形态实测来源：api.lyjxka.top（午夜Free，new-api 系），2026-09-16 用一把
	// 已失效的令牌打 /api/user/self，七个候选头名**全部**返回
	// `HTTP 200 {"message":"Unauthorized, invalid access token","success":false}`。
	//
	// 只在 `success` 这一位**存在且为 false** 时才拦：Sub2API 系用的是
	// `{code,message,data}`，没有这个字段，行为完全不变。
	if ok, exists := m["success"].(bool); exists && !ok {
		return nil, raw, authRejectionError(m, raw, method, path, secret)
	}
	return m, raw, nil
}

func readAuthBody(resp *http.Response, secret bool) ([]byte, error) {
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		var timeout net.Error
		switch {
		case !secret:
			return nil, err
		case errors.Is(err, context.Canceled):
			return nil, context.Canceled
		case errors.As(err, &timeout) && timeout.Timeout():
			return nil, context.DeadlineExceeded
		default:
			return nil, errors.New("响应读取失败")
		}
	}
	if len(raw) > maxBodyBytes {
		return nil, errors.New("响应超过 8 MiB")
	}
	return raw, nil
}

func authHTTPError(resp *http.Response, method, path string, raw []byte, secret bool) error {
	if resp.StatusCode == http.StatusUnauthorized {
		return newHTTPError(resp, fmt.Sprintf("%v: %s %s", ErrUnauthorized, method, path), ErrUnauthorized)
	}
	message := fmt.Sprintf("%s %s 返回 %d", method, path, resp.StatusCode)
	var cause error
	if !secret {
		message += ": " + snippet(raw)
	} else if resp.StatusCode == 403 || (resp.StatusCode >= 300 && resp.StatusCode < 400) {
		cause = ErrCookieNeedsAction
	}
	return newHTTPError(resp, message, cause)
}

func authRejectionError(m map[string]any, raw []byte, method, path string, secret bool) error {
	cause := ErrUpstreamRejected
	// 仅采用 api.lyjxka.top 2026-09-16 已实测的认证错误，不把任意 success:false 当作失效。
	if strings.TrimSpace(asString(m["message"])) == "Unauthorized, invalid access token" {
		cause = errors.Join(cause, ErrInvalidAccessToken)
	}
	if !secret {
		return fmt.Errorf("%w（%s %s）：%s", cause, method, path, upstreamMessage(m, raw))
	}
	if !IsAuthenticationFailure(cause) && path == CookieSessionPath {
		cause = errors.Join(cause, ErrCookieNeedsAction)
	}
	return fmt.Errorf("%s %s: %w", method, path, cause)
}

// CookieSessionPath 是唯一能代表"整份会话"的端点，Cookie 读取侧（collection）共用这一份。
// 其余端点的业务拒绝（单把 Key 在列表与读明文之间被删、某接口被关）只算这一次
// 请求失败，不能把整份 Cookie 标成需人工处理、连带停掉后续所有自动采集。
const CookieSessionPath = "/api/user/self"

// upstreamMessage 取上游自己给的失败说明。
//
// 优先用 message —— 那是人家写给人看的一句话，比我们能编的任何措辞都准。
// 空的时候退回响应片段，**不要退回一句泛泛的"请求失败"**：那等于把上游递到
// 手里的唯一线索丢掉。
func upstreamMessage(m map[string]any, raw []byte) string {
	if msg, _ := m["message"].(string); strings.TrimSpace(msg) != "" {
		return strings.TrimSpace(msg)
	}
	if msg, _ := m["msg"].(string); strings.TrimSpace(msg) != "" {
		return strings.TrimSpace(msg)
	}
	return snippet(raw)
}

var ErrInvalidAccessToken = errors.New("collector: invalid access token")
var ErrCookieUnavailable = errors.New("未配置 Cookie、已停用或部署密钥不可用")
var ErrCookieExpired = errors.New("会话 Cookie 已失效，请在原站登录后重新导入 Cookie")
var ErrCookieNeedsAction = errors.New("需要人工检查 Cookie；不自动登录或处理额外验证")
var ErrCookieIdentityUnavailable = errors.New("无法从此 Cookie 自动识别用户 ID，请导入包含账号信息的 all-api-hub 完整备份")

func IsAuthenticationFailure(err error) bool {
	return errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrInvalidAccessToken)
}

// CookieIdentityMatches 使用已有 NewAPI envelope，但不接受缺失、非整数或其他账号的 ID。
func CookieIdentityMatches(m map[string]any, externalID string) bool {
	expected, err := strconv.ParseInt(externalID, 10, 64)
	if err != nil || expected <= 0 {
		return false
	}
	id := unwrapData(m)["id"]
	if f, ok := id.(float64); ok && (f != math.Trunc(f) || f > 1<<53) {
		return false
	}
	actual, err := strconv.ParseInt(asString(id), 10, 64)
	return err == nil && actual == expected
}
